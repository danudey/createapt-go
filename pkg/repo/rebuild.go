package repo

import (
	"context"
	"io"
	"os"
	"path"
	"strings"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/backend"
	"github.com/danudey/createapt-go/pkg/progress"
)

// PruneReport summarizes a dependency-aware prune. Removed lists the versions
// actually dropped from the index. When a superseded version is kept because
// another package still depends on that specific version, the dependency is
// recorded in Kept (and the version is not removed). When PruneBreakDeps
// permits dropping such versions anyway, the now-broken dependencies are
// recorded in Broken instead.
type PruneReport struct {
	Removed []aptdata.Entry
	Kept    []aptdata.Breakage
	Broken  []aptdata.Breakage
}

// PruneOlderVersions drops entries superseded by a newer version of the same
// name+architecture, keeping only the highest version in each group. A
// superseded version that another package still depends on specifically is kept
// (and reported in PruneReport.Kept) unless PruneBreakDeps is set, in which case
// it is dropped and the broken dependency is reported instead. The files of
// dropped entries are garbage-collected on the next Commit.
func (r *Repo) PruneOlderVersions() PruneReport {
	groups := map[string][]aptdata.Entry{}
	for _, e := range r.idx.Entries() {
		groups[e.Key()] = append(groups[e.Key()], e)
	}
	var candidates []aptdata.Entry
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		best := g[0]
		for _, e := range g[1:] {
			if best.ParsedVersion().Compare(e.ParsedVersion()) < 0 {
				best = e
			}
		}
		for _, e := range g {
			if e != best {
				candidates = append(candidates, e)
			}
		}
	}
	if len(candidates) == 0 {
		return PruneReport{}
	}

	// Only binary packages can break a dependency; a source package has no
	// dependents inside the repository, so an older one is always safe to drop.
	var binaryCandidates []*aptdata.Package
	for _, e := range candidates {
		if p, ok := e.(*aptdata.Package); ok {
			binaryCandidates = append(binaryCandidates, p)
		}
	}
	breakages := aptdata.RemovalBreakages(r.idx.Packages(), binaryCandidates)

	if r.opt.PruneBreakDeps {
		for _, e := range candidates {
			r.idx.RemoveByID(e.ID())
		}
		return PruneReport{Removed: candidates, Broken: breakages}
	}

	protected := make(map[string]bool)
	for _, b := range breakages {
		protected[b.Provider.ID()] = true
	}
	var removed []aptdata.Entry
	for _, e := range candidates {
		if protected[e.ID()] {
			continue
		}
		r.idx.RemoveByID(e.ID())
		removed = append(removed, e)
	}
	return PruneReport{Removed: removed, Kept: breakages}
}

// RelocateAll recomputes every entry's pool location from the current layout
// options, returning the number of entries whose location changed. An entry
// already where it belongs is left untouched, so calling this repeatedly is
// idempotent. Commit performs the actual moves server-side (no re-upload) where
// the backend supports it.
func (r *Repo) RelocateAll() int {
	moved := 0
	for _, p := range r.idx.Packages() {
		want := path.Join(r.poolDir(p.Component, p.SourceName()), path.Base(p.Location()))
		if want != p.Location() {
			p.SetLocation(want)
			moved++
		}
	}
	for _, s := range r.idx.Sources() {
		want := r.poolDir(s.Component, s.Name())
		if want != s.Directory() {
			s.SetDirectory(want)
			moved++
		}
	}
	return moved
}

// poolDir returns the directory a component's files belong in under the current
// layout option.
func (r *Repo) poolDir(component, sourceName string) string {
	if component == "" {
		component = r.opt.component()
	}
	if !r.opt.poolLayout() {
		return path.Join(aptdata.PoolDir, component)
	}
	return aptdata.PoolDirFor(component, sourceName)
}

// RefreshEstimate reports how many packages RefreshFromPackages would have to
// download, and how many bytes that is according to the index. It is zero when
// the packages are local files, which is what makes re-reading them the default
// there.
func (r *Repo) RefreshEstimate() (packages int, bytes int64) {
	if backend.IsLocal(r.be) {
		return 0, 0
	}
	for _, p := range r.idx.Packages() {
		packages++
		bytes += p.Size()
	}
	return packages, bytes
}

// PackagesAreLocal reports whether the repository's files are ordinary files on
// this machine, so re-reading them costs nothing.
func (r *Repo) PackagesAreLocal() bool { return backend.IsLocal(r.be) }

// TotalSize reports the repository's total stored size and object count. When
// the backend can enumerate its objects (a Lister) the figures cover every file
// and listed is true. Otherwise it falls back to the sizes recorded in the
// indexes (package files plus the index files the Release references) and
// listed is false, signalling an approximation.
func (r *Repo) TotalSize(ctx context.Context) (bytes int64, files int, listed bool, err error) {
	if l, ok := r.be.(backend.Lister); ok {
		objs, err := l.List(ctx, "")
		if err != nil {
			return 0, 0, false, err
		}
		for _, o := range objs {
			bytes += o.Size
			files++
		}
		return bytes, files, true, nil
	}
	for _, e := range r.idx.Entries() {
		bytes += e.TotalBytes()
		files += len(e.Locations())
	}
	if r.old != nil {
		for _, f := range r.old.Files {
			bytes += f.Size
			files++
		}
	}
	return bytes, files, false, nil
}

// GetToFile streams the object at relpath from the backend into a new temporary
// file and returns its path. The caller owns the file and must remove it.
func (r *Repo) GetToFile(ctx context.Context, relpath string) (string, error) {
	return r.getToFile(ctx, relpath, nil)
}

// getToFile is GetToFile with a progress task to credit the bytes to. task may
// be nil.
func (r *Repo) getToFile(ctx context.Context, relpath string, task *progress.Task) (string, error) {
	rc, err := r.be.Get(ctx, relpath)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	f, err := os.CreateTemp("", "createapt-*-"+path.Base(relpath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(task.Writer(f), rc); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// SetForce toggles whether Commit overwrites already-present files whose
// content differs from what is staged.
func (r *Repo) SetForce(v bool) { r.opt.Force = v }

// Plan computes the commit Plan without transferring or deleting anything,
// regardless of the DryRun option. It is used to preview a rebuild (including
// the directory-scan cleanups) before asking the operator to confirm.
func (r *Repo) Plan(ctx context.Context) (*Plan, error) {
	saved := r.opt.DryRun
	r.opt.DryRun = true
	defer func() { r.opt.DryRun = saved }()
	return r.Commit(ctx)
}

// DuplicateLocations reports package files that more than one index entry
// claims. Two entries naming one file cannot both be right about its checksum,
// and dropping either would leave the file due for deletion while the other
// still references it, so this is reported rather than resolved.
func (r *Repo) DuplicateLocations() map[string][]aptdata.Entry {
	byLocation := map[string][]aptdata.Entry{}
	for _, e := range r.idx.Entries() {
		for _, loc := range e.Locations() {
			byLocation[loc] = append(byLocation[loc], e)
		}
	}
	out := map[string][]aptdata.Entry{}
	for loc, entries := range byLocation {
		if len(entries) > 1 {
			out[loc] = entries
		}
	}
	return out
}

// isPackageFile reports whether a path names a file a repository's pool holds.
func isPackageFile(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".deb", ".udeb", ".dsc":
		return true
	}
	// Source tarballs carry compound extensions (.orig.tar.gz, .debian.tar.xz).
	return strings.Contains(p, ".tar.")
}
