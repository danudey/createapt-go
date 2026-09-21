package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/backend"
	"github.com/danudey/createapt-go/pkg/debmeta"
	"github.com/danudey/createapt-go/pkg/progress"
)

// ConfigPath is the repo-root-relative name of the createapt-go config file.
// It is duplicated from the repoconfig package (rather than imported) so the
// copy engine can treat it as just another object to transfer without repo
// depending on the config layer.
const ConfigPath = "createapt-go.json"

// Release returns the suite's Release as it was published, or nil for a suite
// that does not exist yet.
func (r *Repo) Release() *aptdata.Release { return r.old }

// ReleaseSignatures fetches the suite's InRelease and Release.gpg, returning
// nil for each that is absent. They are fetched on demand so ordinary
// operations do not pay for the extra round trips.
func (r *Repo) ReleaseSignatures(ctx context.Context) (inRelease, detached []byte, err error) {
	suite := r.opt.suite()
	for _, spec := range []struct {
		path string
		into *[]byte
	}{
		{aptdata.InReleasePath(suite), &inRelease},
		{aptdata.ReleaseGPGPath(suite), &detached},
	} {
		data, err := r.getAll(ctx, spec.path)
		if errors.Is(err, backend.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		*spec.into = data
	}
	return inRelease, detached, nil
}

// RawRelease returns the suite's Release file exactly as published. The bytes
// matter because a detached signature is only valid against the original
// document, not a re-rendered one.
func (r *Repo) RawRelease(ctx context.Context) ([]byte, error) {
	return r.getAll(ctx, aptdata.ReleasePath(r.opt.suite()))
}

// IsSigned reports whether the loaded suite carries a Release signature.
func (r *Repo) IsSigned(ctx context.Context) (bool, error) {
	in, det, err := r.ReleaseSignatures(ctx)
	if err != nil {
		return false, err
	}
	return in != nil || det != nil, nil
}

// ClearIndex drops every entry from the in-memory index while keeping the
// record of what the published indexes referenced, so that files the new
// indexes no longer mention are garbage-collected on Commit. It is used by
// `copy --overwrite`, which replaces a destination suite wholesale.
func (r *Repo) ClearIndex() {
	for _, e := range r.idx.Entries() {
		r.idx.RemoveByID(e.ID())
	}
}

// ObjectKind labels a repository file by the role it plays.
type ObjectKind string

// The kinds of file a repository is made of. Copying treats each kind
// differently: the Release and its signatures are written last, package files
// may be skipped when already present, and anything "other" is carried across
// as-is.
const (
	ObjectRelease    ObjectKind = "release"   // dists/<suite>/Release
	ObjectReleaseSig ObjectKind = "signature" // InRelease, Release.gpg
	ObjectIndex      ObjectKind = "index"     // a Packages/Sources file the Release covers
	ObjectPackage    ObjectKind = "package"   // a pool file an index references
	ObjectConfig     ObjectKind = "config"    // createapt-go.json
	ObjectOther      ObjectKind = "other"     // anything else found in the backend
)

// SourceObject is one file that makes up a repository, together with whatever
// integrity data the metadata records for it. Checksum is empty for files
// nothing vouches for (the Release itself, and any stray file).
type SourceObject struct {
	Path     string
	Kind     ObjectKind
	Size     int64  // -1 when unknown
	Checksum string // lowercase hex sha256
}

// verifiable reports whether the object carries a checksum this tool can check.
func (o SourceObject) verifiable() bool { return o.Checksum != "" }

// Objects enumerates every file that makes up the suite and the pool it draws
// on. When the backend can list its contents, the listing is authoritative and
// files the metadata does not mention are included as ObjectOther; listed is
// then true. Otherwise (a read-only HTTP source, say) the set is derived from
// the metadata alone and listed is false, meaning any file outside the metadata
// is invisible.
func (r *Repo) Objects(ctx context.Context) (objs []SourceObject, listed bool, err error) {
	if r.old == nil {
		return nil, false, fmt.Errorf("no repository metadata loaded")
	}
	suite := r.opt.suite()
	suiteDir := aptdata.SuiteDir(suite)

	known := map[string]SourceObject{}
	add := func(o SourceObject) { known[o.Path] = o }

	add(SourceObject{Path: aptdata.ReleasePath(suite), Kind: ObjectRelease, Size: -1})
	for _, f := range r.old.Files {
		dest := path.Join(suiteDir, f.Path)
		sum := f.Hashes[aptdata.HashSHA256]
		add(SourceObject{Path: dest, Kind: ObjectIndex, Size: f.Size, Checksum: sum})

		// The by-hash copies are not themselves listed in the Release, but
		// their paths follow from the checksums that are, so they can be
		// enumerated even from a source that cannot list its contents. They
		// carry the same bytes as the index they duplicate.
		if r.old.AcquireByHash() {
			for algo, digest := range f.Hashes {
				add(SourceObject{
					Path: aptdata.ByHashPath(path.Dir(dest), algo, digest),
					Kind: ObjectIndex, Size: f.Size, Checksum: sum,
				})
			}
		}
	}
	for _, e := range r.idx.Entries() {
		for _, f := range entryFiles(e) {
			add(SourceObject{Path: f.location, Kind: ObjectPackage, Size: f.size, Checksum: f.sha256})
		}
	}

	if l, ok := r.be.(backend.Lister); ok {
		all, err := l.List(ctx, "")
		if err != nil {
			return nil, false, fmt.Errorf("list %s: %w", r.be, err)
		}
		for _, o := range all {
			// A listing covers the whole repository, including other suites.
			// Only this suite's dists/ tree belongs to this copy; the pool is
			// shared, so pool files are kept only when an index names them.
			if strings.HasPrefix(o.Path, aptdata.DistsDir+"/") && !strings.HasPrefix(o.Path, suiteDir+"/") {
				continue
			}
			if k, ok := known[o.Path]; ok {
				if k.Size < 0 {
					k.Size = o.Size
					known[o.Path] = k
				}
				continue
			}
			if strings.HasPrefix(o.Path, aptdata.PoolDir+"/") {
				// An unreferenced pool file may belong to another suite that
				// shares the pool, so it is not this suite's to copy.
				continue
			}
			known[o.Path] = SourceObject{Path: o.Path, Kind: classifyObject(o.Path, suite), Size: o.Size}
		}
		listed = true
	} else {
		// Without a listing, the files nothing names have to be probed for.
		for _, probe := range []struct {
			path string
			kind ObjectKind
		}{
			{aptdata.InReleasePath(suite), ObjectReleaseSig},
			{aptdata.ReleaseGPGPath(suite), ObjectReleaseSig},
			{ConfigPath, ObjectConfig},
		} {
			fi, err := r.be.Stat(ctx, probe.path)
			if errors.Is(err, backend.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, false, fmt.Errorf("stat %s: %w", probe.path, err)
			}
			add(SourceObject{Path: probe.path, Kind: probe.kind, Size: fi.Size})
		}
	}

	objs = make([]SourceObject, 0, len(known))
	for _, o := range known {
		objs = append(objs, o)
	}
	sortObjects(objs)
	return objs, listed, nil
}

// classifyObject labels a file found by listing but not named by the metadata.
func classifyObject(p, suite string) ObjectKind {
	switch p {
	case aptdata.ReleasePath(suite):
		return ObjectRelease
	case aptdata.InReleasePath(suite), aptdata.ReleaseGPGPath(suite):
		return ObjectReleaseSig
	case ConfigPath:
		return ObjectConfig
	}
	if strings.HasPrefix(p, aptdata.PoolDir+"/") && isPackageFile(p) {
		return ObjectPackage
	}
	if strings.HasPrefix(p, aptdata.SuiteDir(suite)+"/") {
		return ObjectIndex
	}
	return ObjectOther
}

// sortObjects puts the objects into publish order: everything a client might
// need first, then the Release (the atomic publish point), then its signatures.
// Within a group the order is alphabetical, for reproducible output.
func sortObjects(objs []SourceObject) {
	rank := func(o SourceObject) int {
		switch o.Kind {
		case ObjectRelease:
			return 1
		case ObjectReleaseSig:
			return 2
		default:
			return 0
		}
	}
	sort.Slice(objs, func(i, j int) bool {
		if ri, rj := rank(objs[i]), rank(objs[j]); ri != rj {
			return ri < rj
		}
		return objs[i].Path < objs[j].Path
	})
}

// CopyOptions controls a verbatim repository copy.
type CopyOptions struct {
	// DryRun computes the transfer without writing anything.
	DryRun bool
	// Force overwrites a destination object whose content differs from the
	// source's instead of failing.
	Force bool
	// Prune deletes destination objects the source does not have, making the
	// destination an exact replica. It requires a destination that can list its
	// contents.
	Prune bool
	// Inspect is called with the local temporary copy of each transferred
	// object once its checksum has been validated and before it is written to
	// the destination. Returning an error aborts the copy.
	Inspect func(obj SourceObject, localPath string) error
	// Progress reports each object as it is handled. action is one of "copy",
	// "skip" or "delete".
	Progress func(action string, obj SourceObject)

	// Bars, if set, draws progress bars for the transfers. A copy moves each
	// object twice — out of the source and into the destination — so the two
	// passes appear as separate phases of the same task.
	Bars *progress.Bars
}

// CopyStats summarizes what a copy transferred.
type CopyStats struct {
	Copied  int
	Skipped int
	Bytes   int64
	Deleted []string
}

// CopyExact replicates objs from src to dst byte for byte, verifying every
// object whose checksum the metadata records. An object already present at the
// destination with matching content is skipped, which is what makes an
// interrupted copy resumable. Objects are written in the order given, so the
// Release lands last and the destination is never a torn repository.
func CopyExact(ctx context.Context, src, dst backend.Backend, objs []SourceObject, opt CopyOptions) (*CopyStats, error) {
	stats := &CopyStats{}
	report := opt.Progress
	if report == nil {
		report = func(string, SourceObject) {}
	}

	if !opt.DryRun {
		opt.Bars.Start("copying", len(objs), totalSize(objs))
		defer opt.Bars.Finish()
	}

	for _, obj := range objs {
		present, err := destinationMatches(ctx, dst, obj)
		if err != nil {
			return nil, err
		}
		if present && !opt.Force {
			stats.Skipped++
			opt.Bars.Item()
			report("skip", obj)
			continue
		}
		// A dry run reports the transfer without performing it, so it stays
		// cheap even for a repository of any size.
		if opt.DryRun {
			stats.Copied++
			if obj.Size > 0 {
				stats.Bytes += obj.Size
			}
			report("copy", obj)
			continue
		}

		task := opt.Bars.Task(obj.Path, obj.Size)
		local, size, err := fetchToTemp(ctx, src, obj, task)
		if err != nil {
			task.Done()
			return nil, err
		}
		if opt.Inspect != nil {
			if err := opt.Inspect(obj, local); err != nil {
				task.Done()
				os.Remove(local)
				return nil, err
			}
		}
		stats.Copied++
		stats.Bytes += size
		report("copy", obj)
		err = putFile(ctx, dst, obj.Path, local, task)
		task.Done()
		os.Remove(local)
		if err != nil {
			return nil, fmt.Errorf("write %s: %w", obj.Path, err)
		}
	}

	if opt.Prune {
		deleted, err := pruneExtraneous(ctx, dst, objs, opt)
		if err != nil {
			return nil, err
		}
		stats.Deleted = deleted
	}
	return stats, nil
}

// pruneExtraneous removes destination objects the source does not have.
func pruneExtraneous(ctx context.Context, dst backend.Backend, objs []SourceObject, opt CopyOptions) ([]string, error) {
	lister, ok := dst.(backend.Lister)
	if !ok {
		return nil, fmt.Errorf("destination %s cannot list its contents, so extraneous files cannot be removed", dst)
	}
	keep := make(map[string]bool, len(objs))
	for _, o := range objs {
		keep[o.Path] = true
	}
	all, err := lister.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dst, err)
	}
	var deleted []string
	for _, o := range all {
		if keep[o.Path] {
			continue
		}
		deleted = append(deleted, o.Path)
	}
	sort.Strings(deleted)
	if opt.Progress != nil {
		for _, p := range deleted {
			opt.Progress("delete", SourceObject{Path: p, Kind: ObjectOther})
		}
	}
	if opt.DryRun {
		return deleted, nil
	}
	for _, p := range deleted {
		if err := dst.Delete(ctx, p); err != nil {
			return nil, fmt.Errorf("delete %s: %w", p, err)
		}
	}
	return deleted, nil
}

// destinationMatches reports whether dst already holds an object identical to
// the source's. A checksum decides it where one is available (recorded in the
// metadata and computable remotely); otherwise the size has to stand in.
func destinationMatches(ctx context.Context, dst backend.Backend, obj SourceObject) (bool, error) {
	fi, err := dst.Stat(ctx, obj.Path)
	if errors.Is(err, backend.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", obj.Path, err)
	}
	if obj.verifiable() {
		if hasher, ok := dst.(backend.RemoteHasher); ok {
			sum, ok, herr := hasher.Hash(ctx, obj.Path, backend.AlgoSHA256)
			if herr != nil && !errors.Is(herr, backend.ErrNotExist) {
				return false, fmt.Errorf("remote hash %s: %w", obj.Path, herr)
			}
			if ok {
				return strings.EqualFold(sum, obj.Checksum), nil
			}
		}
	}
	switch obj.Kind {
	case ObjectRelease, ObjectReleaseSig, ObjectConfig:
		// Nothing vouches for these, and they are exactly the files whose
		// content changes without changing length — a Release differs from a
		// previous generation only in its Date and checksums. They are small,
		// so they are always rewritten rather than compared by size.
		return false, nil
	case ObjectIndex, ObjectPackage, ObjectOther:
		// Content-addressed or immutable once written, so an equal length means
		// an equal file.
	}
	return obj.Size >= 0 && fi.Size == obj.Size, nil
}

// fetchToTemp streams an object from be into a temporary file, verifying its
// size and (when the metadata records one) its checksum. The caller owns the
// returned file and must remove it. task, when non-nil, is credited with the
// bytes as they arrive.
func fetchToTemp(ctx context.Context, be backend.Backend, obj SourceObject, task *progress.Task) (string, int64, error) {
	task.Phase("download")
	rc, err := be.Get(ctx, obj.Path)
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", obj.Path, err)
	}
	defer rc.Close()

	f, err := os.CreateTemp("", "createapt-copy-*-"+path.Base(obj.Path))
	if err != nil {
		return "", 0, err
	}
	name := f.Name()
	fail := func(err error) (string, int64, error) {
		f.Close()
		os.Remove(name)
		return "", 0, err
	}

	h := sha256.New()
	size, err := io.Copy(task.Writer(io.MultiWriter(f, h)), rc)
	if err != nil {
		return fail(fmt.Errorf("read %s: %w", obj.Path, err))
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", 0, err
	}
	if obj.Size >= 0 && size != obj.Size {
		os.Remove(name)
		return "", 0, fmt.Errorf("%s: got %d bytes, the metadata says %d", obj.Path, size, obj.Size)
	}
	if obj.verifiable() {
		if sum := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(sum, obj.Checksum) {
			os.Remove(name)
			return "", 0, fmt.Errorf("%s: checksum %s does not match the %s the metadata records",
				obj.Path, sum, obj.Checksum)
		}
	}
	return name, size, nil
}

// putFile uploads a local file to a backend. task, when non-nil, is credited
// with the bytes as they go out.
func putFile(ctx context.Context, be backend.Backend, dest, local string, task *progress.Task) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	task.Phase("upload")
	return be.Put(ctx, dest, task.Reader(f), fi.Size())
}

// totalSize sums the objects whose size is known.
func totalSize(objs []SourceObject) int64 {
	var total int64
	for _, o := range objs {
		if o.Size > 0 {
			total += o.Size
		}
	}
	return total
}

// EntryCopyOptions controls CopyEntriesFrom, which brings selected packages
// from another repository into r and regenerates r's indexes for them.
type EntryCopyOptions struct {
	// Relocate recomputes every copied file's pool location from the
	// destination's layout options rather than keeping the source's.
	Relocate bool

	// Component overrides the component copied entries are indexed in. Empty
	// keeps each entry's component from the source.
	Component string

	// RebuildMetadata re-derives each binary package's index stanza from the
	// .deb itself rather than carrying the source's record over. It requires
	// downloading every package, including ones already present at the
	// destination.
	RebuildMetadata bool

	// Inspect is called with each downloaded file once its checksum has been
	// validated and before it is uploaded. Returning an error aborts the copy.
	Inspect func(e aptdata.Entry, localPath string) error

	// Progress reports each file as it is handled. action is "copy" or "skip".
	Progress func(action string, e aptdata.Entry, location string)

	// Bars, if set, draws progress bars for the transfers. Each file is moved
	// twice — downloaded, then uploaded — and the two passes appear as separate
	// phases of the same task.
	Bars *progress.Bars
}

// CopyEntriesFrom transfers entries (records from another repository's indexes)
// out of src and into r, one file at a time: each is streamed to a temporary
// file, checksum-verified, optionally inspected, uploaded, and then removed, so
// a copy of any size needs room for only one package on local disk.
//
// The entries are added to r's index; the caller publishes them with Commit. A
// file already present at the destination with matching content is not
// transferred again, which is what makes an interrupted copy resumable.
func (r *Repo) CopyEntriesFrom(ctx context.Context, src backend.Backend, entries []aptdata.Entry, opt EntryCopyOptions) (*CopyStats, error) {
	stats := &CopyStats{}
	report := opt.Progress
	if report == nil {
		report = func(string, aptdata.Entry, string) {}
	}

	if !r.opt.DryRun {
		opt.Bars.Start("copying", entryFileCount(entries), entryTotalBytes(entries))
		defer opt.Bars.Finish()
	}

	for _, e := range entries {
		indexed, moves, err := r.placeCopy(e, opt)
		if err != nil {
			return nil, err
		}

		for _, m := range moves {
			if err := r.copyOneFile(ctx, src, e, m, opt, stats, report); err != nil {
				return nil, err
			}
		}

		// A binary package whose stanza is rebuilt is re-derived from the file
		// that was just written, so the destination's index describes what it
		// actually holds rather than what the source claimed.
		if opt.RebuildMetadata && !r.opt.DryRun {
			if p, ok := indexed.(*aptdata.Package); ok {
				if err := r.reindexFromBackend(ctx, p, opt.Bars); err != nil {
					return nil, fmt.Errorf("re-index %s: %w", e.ID3(), err)
				}
			}
		}
	}
	return stats, nil
}

// fileMove is one file of a copied entry: where it comes from and where it goes.
type fileMove struct {
	from     string
	to       string
	size     int64
	checksum string
}

// entryFileCount counts the individual files the entries own.
func entryFileCount(entries []aptdata.Entry) int {
	var n int
	for _, e := range entries {
		n += len(e.Locations())
	}
	return n
}

// entryTotalBytes sums the sizes the indexes record for the entries' files.
func entryTotalBytes(entries []aptdata.Entry) int64 {
	var total int64
	for _, e := range entries {
		total += e.TotalBytes()
	}
	return total
}

// copyOneFile transfers a single file of a copied entry.
func (r *Repo) copyOneFile(ctx context.Context, src backend.Backend, e aptdata.Entry, m fileMove, opt EntryCopyOptions, stats *CopyStats, report func(string, aptdata.Entry, string)) error {
	if !opt.RebuildMetadata {
		present, err := destinationMatches(ctx, r.be, SourceObject{
			Path: m.to, Kind: ObjectPackage, Size: m.size, Checksum: m.checksum,
		})
		if err != nil {
			return err
		}
		if present {
			r.copied[m.to] = m.size
			stats.Skipped++
			opt.Bars.Item()
			report("skip", e, m.to)
			return nil
		}
	}
	// A dry run reports the transfer without performing it. The source's own
	// record stands in for the one a rebuild would derive from the file, which
	// is enough to plan against.
	if r.opt.DryRun {
		r.copied[m.to] = m.size
		stats.Copied++
		stats.Bytes += m.size
		report("copy", e, m.to)
		return nil
	}

	task := opt.Bars.Task(m.to, m.size)
	defer task.Done()

	local, size, err := fetchToTemp(ctx, src, SourceObject{
		Path: m.from, Kind: ObjectPackage, Size: m.size, Checksum: m.checksum,
	}, task)
	if err != nil {
		return err
	}
	// Every temporary file is released before the next one is fetched, so disk
	// use stays bounded by the largest single file.
	defer os.Remove(local)

	if opt.Inspect != nil {
		if err := opt.Inspect(e, local); err != nil {
			return err
		}
	}
	stats.Copied++
	stats.Bytes += size
	report("copy", e, m.to)

	if err := putFile(ctx, r.be, m.to, local, task); err != nil {
		return fmt.Errorf("write %s: %w", m.to, err)
	}
	r.copied[m.to] = size
	return nil
}

// placeCopy inserts a copy of a source entry into r's index at its destination
// location, and returns the file moves that copy implies.
func (r *Repo) placeCopy(e aptdata.Entry, opt EntryCopyOptions) (aptdata.Entry, []fileMove, error) {
	component := opt.Component
	if component == "" {
		component = e.ComponentName()
	}
	if component == "" {
		component = r.opt.component()
	}

	switch t := e.(type) {
	case *aptdata.Package:
		cp := &aptdata.Package{Paragraph: t.Clone(), Component: component}
		from := t.Location()
		to := from
		if opt.Relocate || opt.Component != "" {
			to = path.Join(r.poolDir(component, cp.SourceName()), path.Base(from))
		}
		cp.SetLocation(to)
		r.supersedeCopy(cp)
		r.idx.Add(cp)
		return cp, []fileMove{{from: from, to: to, size: t.Size(), checksum: t.ID()}}, nil

	case *aptdata.Source:
		cp := &aptdata.Source{Paragraph: t.Clone(), Component: component}
		fromDir := t.Directory()
		toDir := fromDir
		if opt.Relocate || opt.Component != "" {
			toDir = r.poolDir(component, cp.Name())
		}
		cp.SetDirectory(toDir)
		var moves []fileMove
		for _, f := range t.SourceFiles() {
			moves = append(moves, fileMove{
				from: f.Location(fromDir), to: f.Location(toDir),
				size: f.Size, checksum: f.SHA256,
			})
		}
		r.supersedeCopy(cp)
		r.idx.AddSource(cp)
		return cp, moves, nil

	default:
		return nil, nil, fmt.Errorf("cannot copy entry of unknown kind %T", e)
	}
}

// reindexFromBackend re-derives a copied binary package's stanza from the file
// now at the destination, replacing the record carried over from the source.
func (r *Repo) reindexFromBackend(ctx context.Context, p *aptdata.Package, bars *progress.Bars) error {
	loc := p.Location()
	// The re-read is not part of the announced transfer, so it is shown without
	// being counted a second time.
	task := bars.Aside(loc, p.Size())
	local, cleanup, err := r.localCopy(ctx, loc, task)
	task.Done()
	if err != nil {
		return err
	}
	defer cleanup()

	fresh, err := debmeta.PackageFromFile(local, debmeta.Options{
		Hashes:     r.opt.hashes(),
		Component:  p.Component,
		PoolLayout: r.opt.poolLayout(),
	})
	if err != nil {
		return err
	}
	fresh.SetLocation(loc)
	r.idx.RemoveByID(p.ID())
	r.idx.Add(fresh)
	r.copied[loc] = fresh.Size()
	return nil
}

// supersedeCopy drops any entry already in the index with the same name,
// architecture and version, so an incremental copy replaces rather than
// duplicates.
func (r *Repo) supersedeCopy(e aptdata.Entry) {
	for _, existing := range r.idx.Find(e.Name(), e.Arch(), e.Version()) {
		r.idx.RemoveByID(existing.ID())
	}
}

// SameRepository reports whether dst holds a subset of src's entries, i.e.
// whether dst looks like a partial copy of src rather than an unrelated
// repository. An empty destination qualifies. It returns a descriptive error
// naming an entry that does not belong when it does not.
func SameRepository(src, dst *aptdata.Index) error {
	if dst.Total() == 0 {
		return nil
	}
	for _, e := range dst.Entries() {
		if src.EntryByID(e.ID()) == nil {
			return fmt.Errorf("the destination holds %s, which the source repository does not have", e.ID3())
		}
	}
	return nil
}
