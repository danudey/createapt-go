package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/backend"
	"github.com/danudey/createapt-go/pkg/debmeta"
	"github.com/danudey/createapt-go/pkg/progress"
)

// RefreshResult summarizes a pass of RefreshFromPackages.
type RefreshResult struct {
	// Read counts the packages successfully re-read from their files.
	Read int
	// Changed counts those whose regenerated stanza differed from what was
	// published — the packages whose index entry was actually wrong.
	Changed int
	// Duplicates counts stale index entries dropped because another entry named
	// the same file and matched it.
	Duplicates int

	// Missing lists locations the index references that the backend does not
	// hold. Their entries are left exactly as published.
	Missing []string
	// Unreadable lists locations that could not be parsed as a package. Their
	// entries are left exactly as published.
	Unreadable []string
	// Conflicts describes pairs of locations holding byte-identical packages,
	// which cannot both be indexed. Both entries are left as published.
	Conflicts []string
}

// RefreshOptions configures RefreshFromPackages.
type RefreshOptions struct {
	// Progress, if set, is called once per package as it is re-read, with
	// changed reporting whether the regenerated stanza differed from the
	// published one.
	Progress func(p *aptdata.Package, changed bool)
}

// RefreshFromPackages re-reads every indexed binary package from its published
// file and regenerates its index stanza from the file itself, so the index
// describes the package that is actually there.
//
// This exists because a package file can be replaced under the same name — a
// rebuilt .deb published over the old one — leaving the index describing the
// build it superseded. apt then reports a size or hash mismatch when it tries
// to download the package, which a user sees as a broken repository rather than
// as a stale index.
//
// Re-reading is free when the packages are local files and costs a full
// download when they are not, so the caller decides whether to call it.
//
// Source packages are not re-read: a .dsc carries its own checksums for its
// tarballs, so the Sources index only restates what the .dsc already asserts,
// and re-deriving it would mean downloading every tarball to learn nothing new.
// A source package whose files have changed underneath it is caught by verify
// instead.
//
// Three situations are reported rather than silently resolved, because each one
// means the repository holds something the index cannot faithfully describe:
// a missing file, an unreadable one, and two locations holding byte-identical
// packages (indexing either would leave the other unreferenced and due for
// deletion).
func (r *Repo) RefreshFromPackages(ctx context.Context, opt RefreshOptions) (*RefreshResult, error) {
	res := &RefreshResult{}

	// Re-read each package, keyed by the location it was published at. Several
	// index entries may name one location; they collapse onto whichever entry
	// the file actually matches.
	type reread struct {
		fresh *aptdata.Package
		was   []*aptdata.Package
	}
	byLocation := map[string]*reread{}
	var order []string

	for _, p := range r.idx.Packages() {
		loc := p.Location()
		if entry, seen := byLocation[loc]; seen {
			entry.was = append(entry.was, p)
			continue
		}
		byLocation[loc] = &reread{was: []*aptdata.Package{p}}
		order = append(order, loc)
	}
	sort.Strings(order)

	// Re-reading a remote repository downloads every package, so it is exactly
	// the kind of run progress is wanted for.
	var totalBytes int64
	for _, loc := range order {
		totalBytes += byLocation[loc].was[0].Size()
	}
	r.opt.Progress.Start("re-reading packages", len(order), totalBytes)
	defer r.opt.Progress.Finish()

	for _, loc := range order {
		entry := byLocation[loc]
		published := entry.was[0]

		task := r.opt.Progress.Task(loc, published.Size())
		local, cleanup, err := r.localCopy(ctx, loc, task)
		if errors.Is(err, backend.ErrNotExist) {
			task.Done()
			res.Missing = append(res.Missing, loc)
			continue
		}
		if err != nil {
			task.Done()
			return nil, fmt.Errorf("read %s: %w", loc, err)
		}
		fresh, err := debmeta.PackageFromFile(local, debmeta.Options{
			Hashes:     r.opt.hashes(),
			Component:  published.Component,
			PoolLayout: r.opt.poolLayout(),
		})
		cleanup()
		task.Done()
		if err != nil {
			res.Unreadable = append(res.Unreadable, loc)
			continue
		}

		// Keep the file where it is. Relocation is RelocateAll's job, and a
		// refresh that also moved files would make the two impossible to reason
		// about separately.
		fresh.SetLocation(loc)
		entry.fresh = fresh
		res.Read++
	}

	// Two locations holding byte-identical packages would collapse onto one
	// index entry, leaving the other file unreferenced and due for deletion.
	// That is a decision for the operator, so both are left as published.
	conflicting := map[string]bool{}
	firstAt := map[string]string{}
	for _, loc := range order {
		entry := byLocation[loc]
		if entry.fresh == nil {
			continue
		}
		id := entry.fresh.ID()
		if other, seen := firstAt[id]; seen {
			res.Conflicts = append(res.Conflicts,
				fmt.Sprintf("%s and %s hold the same package (%s)", other, loc, entry.fresh.ID3()))
			conflicting[other] = true
			conflicting[loc] = true
			continue
		}
		firstAt[id] = loc
	}

	for _, loc := range order {
		entry := byLocation[loc]
		if entry.fresh == nil || conflicting[loc] {
			continue
		}
		published := entry.was[0]
		changed := entry.fresh.ID() != published.ID() || entry.fresh.Size() != published.Size()

		// A stale entry left beside the current one by a bad publish names the
		// same file; only the entry the file matches survives. No file is
		// deleted by this, because the surviving entry still names it.
		for _, old := range entry.was {
			r.idx.RemoveByID(old.ID())
		}
		if len(entry.was) > 1 {
			res.Duplicates += len(entry.was) - 1
		}
		r.idx.Add(entry.fresh)

		if changed {
			res.Changed++
		}
		if opt.Progress != nil {
			opt.Progress(entry.fresh, changed)
		}
	}

	sort.Strings(res.Missing)
	sort.Strings(res.Unreadable)
	sort.Strings(res.Conflicts)
	return res, nil
}

// localCopy makes an indexed file readable as a local path. On a backend whose
// objects are already files on this machine it returns the path in place and a
// no-op cleanup; otherwise it downloads to a temporary file the caller must
// release through the returned cleanup.
func (r *Repo) localCopy(ctx context.Context, relpath string, task *progress.Task) (string, func(), error) {
	if p, ok := backend.LocalPath(r.be, relpath); ok {
		// A FileStore reports the path of an object whether or not it exists,
		// so absence has to be established separately.
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) {
				return "", func() {}, backend.ErrNotExist
			}
			return "", func() {}, err
		}
		return p, func() {}, nil
	}
	tmp, err := r.getToFile(ctx, relpath, task)
	if err != nil {
		return "", func() {}, err
	}
	return tmp, func() { _ = os.Remove(tmp) }, nil
}
