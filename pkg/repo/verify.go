package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/backend"
)

// Problem is one discrepancy found while verifying a published repository.
type Problem struct {
	// Kind groups the finding: "package", "dependency".
	Kind string
	// Subject names what is wrong — a package identifier, or a dependency.
	Subject string
	// Location is the repo-relative path involved, when there is one.
	Location string
	// Detail explains the discrepancy.
	Detail string
}

func (p Problem) String() string {
	if p.Location != "" {
		return fmt.Sprintf("%s: %s (%s)", p.Subject, p.Detail, p.Location)
	}
	return fmt.Sprintf("%s: %s", p.Subject, p.Detail)
}

// ChecksumMode selects how thoroughly Verify checks that each published file's
// content matches the checksum the indexes record.
type ChecksumMode int

const (
	// ChecksumCheap uses only a checksum the backend can supply without
	// transferring the object. That is a real content hash on local disk and
	// over SSH, but on an object store it is the checksum recorded at upload
	// time, and on plain HTTP there is none at all. Files whose checksum could
	// not be confirmed from content are counted in
	// VerifyResult.ChecksumUnconfirmed rather than reported as problems.
	ChecksumCheap ChecksumMode = iota

	// ChecksumContent proves every file's checksum from its bytes, downloading
	// it when the backend cannot hash content itself. It is the only mode that
	// detects an object whose content changed after it was published.
	ChecksumContent
)

// VerifyOptions configures Verify.
type VerifyOptions struct {
	// Checksums selects how file checksums are established.
	Checksums ChecksumMode

	// Concurrency bounds how many files are checked at once. Zero means 1. It
	// matters most in ChecksumContent mode against a remote backend, where each
	// file is a separate transfer.
	Concurrency int

	// SkipDependencies omits the intra-repository dependency check.
	SkipDependencies bool

	// Progress, if set, is called once per entry as its check finishes. It may
	// be called from several goroutines at once.
	Progress func(e aptdata.Entry, problems int)
}

// VerifyResult summarizes a verification pass.
type VerifyResult struct {
	// Packages and Sources count the indexed entries.
	Packages int
	Sources  int
	// Files counts the individual published files checked. It exceeds
	// Packages+Sources because a source package owns several files.
	Files int

	// ChecksumVerified counts files whose checksum was proved from content.
	ChecksumVerified int
	// ChecksumRecorded counts files whose checksum was confirmed only against a
	// value the backend recorded at upload time.
	ChecksumRecorded int
	// ChecksumUnconfirmed counts files whose checksum could not be established
	// at all (only their size was checked).
	ChecksumUnconfirmed int
	// Downloaded and BytesRead count the files transferred to hash them.
	Downloaded int
	BytesRead  int64

	Problems []Problem
}

// OK reports whether the repository verified cleanly.
func (r *VerifyResult) OK() bool { return len(r.Problems) == 0 }

// DownloadEstimate reports how many files Verify would have to transfer to
// prove their checksums from content, and how many bytes that is according to
// the indexes. It lets a caller warn before starting a long download. It is
// zero when the backend can hash content on its own.
func (r *Repo) DownloadEstimate() (files int, bytes int64) {
	if backend.HashesContent(r.be) {
		return 0, 0
	}
	for _, e := range r.idx.Entries() {
		files += len(e.Locations())
		bytes += e.TotalBytes()
	}
	return files, bytes
}

// Verify checks that every file the indexes reference is present in the backend
// with the size and checksum they claim, and that the repository's
// intra-repository dependencies are satisfied. It never modifies anything.
func (r *Repo) Verify(ctx context.Context, opt VerifyOptions) (*VerifyResult, error) {
	entries := r.idx.Entries()
	res := &VerifyResult{Packages: r.idx.Len(), Sources: r.idx.SourceLen()}

	conc := opt.Concurrency
	if conc < 1 {
		conc = 1
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, conc)
	)
	for _, e := range entries {
		wg.Add(1)
		sem <- struct{}{}
		go func(e aptdata.Entry) {
			defer wg.Done()
			defer func() { <-sem }()
			one := r.verifyEntry(ctx, e, opt)

			mu.Lock()
			res.Problems = append(res.Problems, one.problems...)
			res.Files += one.files
			res.ChecksumVerified += one.verified
			res.ChecksumRecorded += one.recorded
			res.ChecksumUnconfirmed += one.unconfirmed
			res.Downloaded += one.downloaded
			res.BytesRead += one.bytesRead
			mu.Unlock()

			if opt.Progress != nil {
				opt.Progress(e, len(one.problems))
			}
		}(e)
	}
	wg.Wait()

	if !opt.SkipDependencies {
		for _, dp := range r.CheckDependencies() {
			res.Problems = append(res.Problems, Problem{
				Kind:     "dependency",
				Subject:  dp.Package.ID3(),
				Location: dp.Package.Location(),
				Detail:   dp.String(),
			})
		}
	}

	sort.Slice(res.Problems, func(i, j int) bool {
		a, b := res.Problems[i], res.Problems[j]
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		return a.Detail < b.Detail
	})
	return res, nil
}

// entryVerdict is one entry's contribution to a VerifyResult.
type entryVerdict struct {
	problems    []Problem
	files       int
	verified    int
	recorded    int
	unconfirmed int
	downloaded  int
	bytesRead   int64
}

func (v *entryVerdict) fail(e aptdata.Entry, location, detail string) {
	v.problems = append(v.problems, Problem{
		Kind: "package", Subject: e.ID3(), Location: location, Detail: detail,
	})
}

// verifyEntry checks every file an entry owns. A binary package owns one; a
// source package owns its .dsc and its tarballs, each of which is checked
// against the size and checksum the Sources index records for it.
func (r *Repo) verifyEntry(ctx context.Context, e aptdata.Entry, opt VerifyOptions) entryVerdict {
	var v entryVerdict
	for _, f := range entryFiles(e) {
		v.files++
		r.verifyFile(ctx, e, f, opt, &v)
	}
	return v
}

// verifiableFile is one published file with what the indexes say about it.
type verifiableFile struct {
	location string
	size     int64
	sha256   string
}

// entryFiles lists an entry's files with their expected size and checksum.
func entryFiles(e aptdata.Entry) []verifiableFile {
	switch t := e.(type) {
	case *aptdata.Package:
		return []verifiableFile{{location: t.Location(), size: t.Size(), sha256: t.ID()}}
	case *aptdata.Source:
		dir := t.Directory()
		files := t.SourceFiles()
		out := make([]verifiableFile, 0, len(files))
		for _, f := range files {
			out = append(out, verifiableFile{location: f.Location(dir), size: f.Size, sha256: f.SHA256})
		}
		return out
	default:
		return nil
	}
}

// verifyFile checks one published file's presence, size and checksum.
func (r *Repo) verifyFile(ctx context.Context, e aptdata.Entry, f verifiableFile, opt VerifyOptions, v *entryVerdict) {
	fi, err := r.be.Stat(ctx, f.location)
	switch {
	case errors.Is(err, backend.ErrNotExist):
		v.fail(e, f.location, "the file the index references is missing")
		return
	case err != nil:
		v.fail(e, f.location, "stat failed: "+err.Error())
		return
	}
	if f.size > 0 && fi.Size != f.size {
		v.fail(e, f.location, fmt.Sprintf("size %d does not match the %d the index records", fi.Size, f.size))
		// Keep going: the checksum tells the operator whether the file is a
		// different build or a damaged one.
	}

	if f.sha256 == "" {
		v.unconfirmed++
		return
	}

	// Content is authoritative. Use the backend's own hash only when it is
	// computed from the stored bytes; otherwise fall back to reading the object.
	if backend.HashesContent(r.be) {
		hasher := r.be.(backend.RemoteHasher)
		sum, ok, err := hasher.Hash(ctx, f.location, backend.AlgoSHA256)
		switch {
		case err != nil && !errors.Is(err, backend.ErrNotExist):
			v.fail(e, f.location, "checksum could not be computed: "+err.Error())
			return
		case ok:
			if !strings.EqualFold(sum, f.sha256) {
				v.fail(e, f.location, fmt.Sprintf("content checksum %s does not match the %s the index records", sum, f.sha256))
			} else {
				v.verified++
			}
			return
		}
		// ok == false: the backend could not answer after all; fall through.
	}

	if opt.Checksums != ChecksumContent {
		// Cheap mode: accept a recorded checksum as corroboration, and record
		// honestly when there is nothing to go on.
		if hasher, isHasher := r.be.(backend.RemoteHasher); isHasher {
			sum, ok, err := hasher.Hash(ctx, f.location, backend.AlgoSHA256)
			if err != nil && !errors.Is(err, backend.ErrNotExist) {
				v.fail(e, f.location, "recorded checksum could not be read: "+err.Error())
				return
			}
			if ok {
				if !strings.EqualFold(sum, f.sha256) {
					v.fail(e, f.location, fmt.Sprintf("the checksum %s recorded by the store does not match the %s the index records", sum, f.sha256))
				} else {
					v.recorded++
				}
				return
			}
		}
		v.unconfirmed++
		return
	}

	// Content mode against a backend that cannot hash for us: read the object.
	sum, n, err := hashObject(ctx, r.be, f.location)
	v.downloaded++
	v.bytesRead += n
	if err != nil {
		v.fail(e, f.location, "reading the file to check its checksum: "+err.Error())
		return
	}
	if f.size > 0 && n != f.size {
		v.fail(e, f.location, fmt.Sprintf("read %d bytes, but the index records a size of %d", n, f.size))
	}
	if !strings.EqualFold(sum, f.sha256) {
		v.fail(e, f.location, fmt.Sprintf("content checksum %s does not match the %s the index records", sum, f.sha256))
		return
	}
	v.verified++
}

// hashObject streams an object from the backend through sha256, returning the
// hex digest and the number of bytes read. Nothing is buffered: a package of
// any size costs one pass and no disk.
func hashObject(ctx context.Context, be backend.Backend, relpath string) (string, int64, error) {
	rc, err := be.Get(ctx, relpath)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
