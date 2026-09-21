// Package repo provides high-level repository management on top of a storage
// backend: it loads an existing suite's apt metadata, lets callers add or
// remove packages, and publishes the result while transferring as little data
// as possible. Existing package files are never downloaded; only the (small)
// indexes are fetched, regenerated locally, and written back, and a package
// that is already present is validated remotely (by checksum) rather than
// re-uploaded.
package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/backend"
	"github.com/danudey/createapt-go/pkg/debmeta"
	"github.com/danudey/createapt-go/pkg/progress"
)

// Signer signs a suite's Release file. apt accepts either an inline-signed
// InRelease or a detached Release.gpg beside the plain Release; this tool
// writes both, so every client finds a form it understands.
type Signer interface {
	// SignDetached returns an ASCII-armored detached signature over data.
	SignDetached(data []byte) ([]byte, error)
	// SignClearsigned returns data wrapped in an OpenPGP clearsigned document.
	SignClearsigned(data []byte) ([]byte, error)
}

// Options configures repository operations.
type Options struct {
	// Suite is the dists/ subdirectory operated on, e.g. "stable" or "jammy".
	// It defaults to DefaultSuite.
	Suite string
	// Component is the section newly added packages are placed in. It defaults
	// to DefaultComponent. Packages already indexed keep their own component.
	Component string

	// Architectures fixes the architectures the suite publishes an index for.
	//
	// Left empty, the set is the architectures of the indexed packages plus any
	// the previously published Release listed, so an architecture never
	// disappears out from under a client that is subscribed to it — including
	// when the last package for it is removed. Set it to drop an architecture
	// deliberately.
	Architectures []string

	// Hashes are the digest algorithms written into the indexes and the
	// Release file. Defaults to aptdata.DefaultHashes.
	Hashes []string
	// Compressions are the forms each index is published in. Defaults to
	// aptdata.DefaultCompressions.
	Compressions []aptdata.Compression

	// FlatPool places packages directly under pool/<component>/ instead of the
	// conventional pool/<component>/<prefix>/<source>/ tree.
	//
	// This option, and NoByHash below, are named for what they turn off so the
	// zero Options value produces the repository layout that should be wanted:
	// a library caller who sets nothing gets the conventional pool and the
	// near-atomic publish, rather than silently losing both.
	FlatPool bool

	// NoByHash stops each index being published under its checksum as well as
	// its plain name, and drops Acquire-By-Hash from the Release file. Doing so
	// gives up the near-atomic publish described on Commit, so it is off by
	// default; set it only for a client that cannot read by-hash paths.
	NoByHash bool

	// Validity sets the Release file's Valid-Until this far after its Date. A
	// zero validity writes no Valid-Until, so the Release never expires.
	Validity time.Duration

	// Release file identity. Empty fields are left out, except that Suite is
	// always written.
	Origin         string
	Label          string
	Codename       string
	ReleaseVersion string
	Description    string

	Create     bool // permit creating a suite when none exists
	DryRun     bool // compute and report the plan without transferring
	Force      bool // overwrite a colliding remote file of differing content
	PruneOlder bool // on add, drop older versions of the same name+arch

	// PruneBreakDeps permits pruning (or superseding) a version even when
	// another package in the repository still depends on that specific version.
	// Without it such a version is kept and a warning is recorded instead.
	PruneBreakDeps bool

	// RemoveUnreferencedPackages deletes package files present in the backend
	// that the published indexes do not reference (requires a Lister backend).
	RemoveUnreferencedPackages bool
	// RemoveStaleMetadata deletes files under the suite's dists/ directory that
	// the new Release does not reference (requires a Lister backend).
	RemoveStaleMetadata bool

	// Signer, if set, signs the Release file.
	Signer Signer

	// Progress, if set, draws progress bars for the transfers a publish
	// performs. A nil *progress.Bars is a no-op, which is what a run that was
	// not asked for progress passes.
	Progress *progress.Bars

	// now overrides the timestamp source (for tests); zero means time.Now.
	now int64
}

// Defaults applied when the corresponding option is empty.
const (
	// DefaultSuite is the suite operated on when none is named. "stable" is
	// what a single-suite repository conventionally calls itself.
	DefaultSuite = "stable"
	// DefaultComponent is the section packages land in when none is named.
	DefaultComponent = "main"
)

func (o *Options) suite() string {
	if o.Suite == "" {
		return DefaultSuite
	}
	return o.Suite
}

func (o *Options) component() string {
	if o.Component == "" {
		return DefaultComponent
	}
	return o.Component
}

func (o *Options) hashes() []string {
	if len(o.Hashes) == 0 {
		return aptdata.DefaultHashes
	}
	return o.Hashes
}

// poolLayout reports whether packages go in the conventional pool tree.
func (o *Options) poolLayout() bool { return !o.FlatPool }

// byHash reports whether indexes are also published under their checksums.
func (o *Options) byHash() bool { return !o.NoByHash }

func (o *Options) compressions() []aptdata.Compression {
	if len(o.Compressions) == 0 {
		return aptdata.DefaultCompressions
	}
	return o.Compressions
}

// Repo is a loaded suite ready for mutation and publishing.
type Repo struct {
	be  backend.Backend
	opt Options
	idx *aptdata.Index
	old *aptdata.Release // previously published Release, for metadata GC

	// pending tracks local files to upload, keyed by destination path.
	pending map[string]string // dest -> local path
	// copied records files written to the backend out of band of the staged
	// upload path: the copy command streams each package straight through
	// rather than holding every temporary file until Commit. Commit treats
	// these destinations as already satisfied. It maps dest to size.
	copied map[string]int64
	// original records the package files referenced by the indexes as loaded
	// (dest -> entry id). Commit diffs this against the final index to find
	// files that are no longer referenced (so they can be relocated or deleted).
	original map[string]string

	// pruneKept and pruneBroken accumulate the dependency breakages seen while
	// Add superseded older versions: kept lists versions retained because a
	// dependent still needs them (the default), and broken lists versions
	// dropped anyway because PruneBreakDeps is set. See PruneWarnings.
	pruneKept   []aptdata.Breakage
	pruneBroken []aptdata.Breakage
}

// Backend exposes the underlying storage (for diagnostics).
func (r *Repo) Backend() backend.Backend { return r.be }

// Index exposes the in-memory package set.
func (r *Repo) Index() *aptdata.Index { return r.idx }

// Suite returns the suite this repository operates on.
func (r *Repo) Suite() string { return r.opt.suite() }

// Options returns the effective options.
func (r *Repo) Options() Options { return r.opt }

// SetPruneOlder toggles whether Add also drops older versions of the same
// name+arch when a newer one is added.
func (r *Repo) SetPruneOlder(v bool) { r.opt.PruneOlder = v }

// Open creates a backend for location and loads any existing metadata. If no
// repository exists and opts.Create is false, Open returns an error.
func Open(ctx context.Context, location string, opts Options) (*Repo, error) {
	be, err := backend.Open(ctx, location)
	if err != nil {
		return nil, err
	}
	return OpenWith(ctx, be, opts)
}

// OpenWith is like Open but uses a caller-supplied backend. It is useful for
// embedding (custom backends) and testing.
func OpenWith(ctx context.Context, be backend.Backend, opts Options) (*Repo, error) {
	if err := aptdata.ValidSuite(opts.suite()); err != nil {
		return nil, err
	}
	if err := aptdata.ValidComponent(opts.component()); err != nil {
		return nil, err
	}
	r := &Repo{
		be:       be,
		opt:      opts,
		idx:      aptdata.NewIndex(opts.suite()),
		pending:  map[string]string{},
		copied:   map[string]int64{},
		original: map[string]string{},
	}
	if err := r.load(ctx); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

// Close releases any backend resources.
func (r *Repo) Close() error {
	if c, ok := r.be.(backend.Closer); ok {
		return c.Close()
	}
	return nil
}

// load fetches and parses the suite's existing metadata into the index. A suite
// with no Release file is treated as empty (only allowed when opts.Create is
// set).
func (r *Repo) load(ctx context.Context) error {
	suite := r.opt.suite()

	rel, err := r.loadRelease(ctx, suite)
	if errors.Is(err, backend.ErrNotExist) {
		if !r.opt.Create {
			return fmt.Errorf("no %s suite at %s (use create to initialize)", suite, r.be)
		}
		return nil
	}
	if err != nil {
		return err
	}
	r.old = rel
	r.idx.Release = rel.Para.Clone()

	for _, ref := range indexRefs(rel) {
		data, err := r.fetchIndex(ctx, suite, ref)
		if err != nil {
			return err
		}
		if err := r.ingestIndex(ref, data); err != nil {
			return err
		}
	}

	// Snapshot the package locations the published indexes reference, so Commit
	// can detect files that mutation leaves unreferenced.
	for loc, id := range r.idx.Locations() {
		r.original[loc] = id
	}
	return nil
}

// loadRelease reads a suite's Release file, preferring the inline-signed
// InRelease because it is the document apt itself trusts, and falling back to
// the plain Release.
func (r *Repo) loadRelease(ctx context.Context, suite string) (*aptdata.Release, error) {
	data, err := r.getAll(ctx, aptdata.InReleasePath(suite))
	if err == nil {
		return aptdata.ParseRelease(debmeta.StripClearsign(data))
	}
	if !errors.Is(err, backend.ErrNotExist) {
		return nil, err
	}
	data, err = r.getAll(ctx, aptdata.ReleasePath(suite))
	if err != nil {
		return nil, err
	}
	return aptdata.ParseRelease(data)
}

// indexRef names one index the Release covers, resolved to the concrete file to
// fetch.
type indexRef struct {
	Component string
	Arch      string // a real architecture, or aptdata.ArchSource
	Path      string // relative to the suite directory
	Hashes    aptdata.Hashes
	Size      int64
}

// indexRefs picks, for each component and architecture the Release covers, the
// single index file to fetch. Where the Release lists several compressed forms
// of the same index, the smallest is chosen: they decode to the same document,
// so the cheapest transfer wins.
func indexRefs(rel *aptdata.Release) []indexRef {
	best := map[string]indexRef{}
	for _, f := range rel.Files {
		component, arch, ok := parseIndexPath(f.Path)
		if !ok {
			continue
		}
		key := component + "/" + arch
		candidate := indexRef{
			Component: component, Arch: arch, Path: f.Path,
			Hashes: f.Hashes, Size: f.Size,
		}
		if cur, seen := best[key]; !seen || betterIndex(candidate, cur) {
			best[key] = candidate
		}
	}
	out := make([]indexRef, 0, len(best))
	for _, k := range sortedMapKeys(best) {
		out = append(out, best[k])
	}
	return out
}

// betterIndex prefers the smaller of two encodings of the same index, and
// among equal sizes the one this tool can decode without a shell.
func betterIndex(a, b indexRef) bool {
	if a.Size != b.Size {
		return a.Size > 0 && (b.Size == 0 || a.Size < b.Size)
	}
	return a.Path < b.Path
}

// parseIndexPath recognizes the two index paths a Release covers that this tool
// reads: <component>/binary-<arch>/Packages[.ext] and
// <component>/source/Sources[.ext]. Anything else (a per-component Release, a
// Contents or Translation file, a pdiff index) is not a package index and is
// skipped.
//
// A component may itself contain a slash ("updates/main"), so the architecture
// segment is located from the end rather than by counting from the front.
func parseIndexPath(p string) (component, arch string, ok bool) {
	segments := strings.Split(p, "/")
	if len(segments) < 3 {
		return "", "", false
	}
	base := segments[len(segments)-1]
	dir := segments[len(segments)-2]
	component = strings.Join(segments[:len(segments)-2], "/")
	if component == "" {
		return "", "", false
	}

	switch {
	case dir == aptdata.ArchSource:
		if stripIndexExt(base) != "Sources" {
			return "", "", false
		}
		return component, aptdata.ArchSource, true
	case strings.HasPrefix(dir, "binary-"):
		if stripIndexExt(base) != "Packages" {
			return "", "", false
		}
		return component, strings.TrimPrefix(dir, "binary-"), true
	default:
		return "", "", false
	}
}

// stripIndexExt removes a compression extension from an index filename.
func stripIndexExt(base string) string {
	switch strings.ToLower(path.Ext(base)) {
	case ".gz", ".xz", ".zst", ".zstd", ".bz2", ".lzma":
		return strings.TrimSuffix(base, path.Ext(base))
	default:
		return base
	}
}

// fetchIndex reads an index the Release covers and decompresses it, checking it
// against the checksum the Release records. A mismatch means the repository is
// inconsistent with its own Release, which must not be quietly rebuilt over.
func (r *Repo) fetchIndex(ctx context.Context, suite string, ref indexRef) ([]byte, error) {
	full := path.Join(aptdata.SuiteDir(suite), ref.Path)
	raw, err := r.getAll(ctx, full)
	if errors.Is(err, backend.ErrNotExist) && r.old.AcquireByHash() {
		// A publish interrupted between writing the by-hash copies and the
		// plain index leaves the Release pointing at a file that is present
		// only under its checksum. Reading it there recovers the same bytes.
		if digest, ok := ref.Hashes[aptdata.HashSHA256]; ok {
			byHash := aptdata.ByHashPath(path.Dir(full), aptdata.HashSHA256, digest)
			raw, err = r.getAll(ctx, byHash)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("fetch index %s: %w", ref.Path, err)
	}
	if err := verifyIndexBytes(ref, raw); err != nil {
		return nil, fmt.Errorf("index %s: %w", ref.Path, err)
	}
	data, err := aptdata.Decompress(ref.Path, raw)
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", ref.Path, err)
	}
	return data, nil
}

// verifyIndexBytes checks raw against the size and digests the Release records.
func verifyIndexBytes(ref indexRef, raw []byte) error {
	if ref.Size > 0 && int64(len(raw)) != ref.Size {
		return fmt.Errorf("is %d bytes but the Release records %d", len(raw), ref.Size)
	}
	algos := ref.Hashes.Algorithms()
	if len(algos) == 0 {
		return nil
	}
	got := aptdata.HashBytes(raw, algos)
	for _, algo := range algos {
		if !strings.EqualFold(got[algo], ref.Hashes[algo]) {
			return fmt.Errorf("%s is %s but the Release records %s", algo, got[algo], ref.Hashes[algo])
		}
	}
	return nil
}

// ingestIndex parses an index document into the in-memory package set.
func (r *Repo) ingestIndex(ref indexRef, data []byte) error {
	if ref.Arch == aptdata.ArchSource {
		srcs, err := aptdata.ParseSources(data, ref.Component)
		if err != nil {
			return fmt.Errorf("%s: %w", ref.Path, err)
		}
		for _, s := range srcs {
			r.idx.AddSource(s)
		}
		return nil
	}
	pkgs, err := aptdata.ParsePackages(data, ref.Component)
	if err != nil {
		return fmt.Errorf("%s: %w", ref.Path, err)
	}
	// An arch:all package appears in every architecture's index; the index
	// keys by checksum, so the repeats collapse onto one entry.
	for _, p := range pkgs {
		r.idx.Add(p)
	}
	return nil
}

// AddDeb parses a local .deb, places it at its pool destination and stages it
// for upload. A package with the same name+version+arch already present is
// treated as an update (superseded). With PruneOlder, older versions are also
// removed.
func (r *Repo) AddDeb(localPath string) (*aptdata.Package, error) {
	pkg, err := debmeta.PackageFromFile(localPath, debmeta.Options{
		Hashes:     r.opt.hashes(),
		Component:  r.opt.component(),
		PoolLayout: r.opt.poolLayout(),
	})
	if err != nil {
		return nil, err
	}
	r.supersede(pkg)
	r.idx.Add(pkg)
	r.pending[pkg.Location()] = localPath
	return pkg, nil
}

// AddDSC parses a local .dsc, places its files at their pool destination and
// stages them all for upload.
func (r *Repo) AddDSC(localPath string) (*aptdata.Source, error) {
	sf, err := debmeta.SourceFromFile(localPath, debmeta.Options{
		Hashes:     r.opt.hashes(),
		Component:  r.opt.component(),
		PoolLayout: r.opt.poolLayout(),
	})
	if err != nil {
		return nil, err
	}
	r.supersede(sf.Source)
	r.idx.AddSource(sf.Source)
	for dest, local := range sf.Files {
		r.pending[dest] = local
	}
	return sf.Source, nil
}

// Add dispatches on the file's extension, so a caller can hand it whatever the
// user named.
func (r *Repo) Add(localPath string) (aptdata.Entry, error) {
	switch {
	case debmeta.IsDSC(localPath):
		return r.AddDSC(localPath)
	default:
		return r.AddDeb(localPath)
	}
}

// supersede removes the entries a newly added one replaces: always the same
// version (a rebuild of one build over another, which is safe because the new
// file is about to be published at the same place), and with PruneOlder every
// older version of the same name+arch too.
func (r *Repo) supersede(incoming aptdata.Entry) {
	var pruneCandidates []*aptdata.Package
	incomingVersion := incoming.ParsedVersion()

	for _, existing := range r.idx.Find(incoming.Name(), incoming.Arch(), "") {
		switch {
		case existing.ParsedVersion().Compare(incomingVersion) == 0:
			r.idx.RemoveByID(existing.ID())
		case r.opt.PruneOlder && existing.ParsedVersion().Compare(incomingVersion) < 0:
			if p, ok := existing.(*aptdata.Package); ok {
				pruneCandidates = append(pruneCandidates, p)
				break
			}
			// A source package has no dependents within the repository, so
			// there is nothing to break by dropping an older one.
			r.idx.RemoveByID(existing.ID())
		}
	}
	if len(pruneCandidates) == 0 {
		return
	}

	// Evaluate the prune against the package set as it will exist once the
	// incoming package is added, so a newer version being introduced can
	// satisfy dependents that the older one was holding up.
	all := r.idx.Packages()
	if p, ok := incoming.(*aptdata.Package); ok {
		all = append(all, p)
	}
	breakages := aptdata.RemovalBreakages(all, pruneCandidates)
	if r.opt.PruneBreakDeps {
		for _, p := range pruneCandidates {
			r.idx.RemoveByID(p.ID())
		}
		r.pruneBroken = append(r.pruneBroken, breakages...)
		return
	}
	protected := make(map[string]bool)
	for _, b := range breakages {
		protected[b.Provider.ID()] = true
	}
	for _, p := range pruneCandidates {
		if !protected[p.ID()] {
			r.idx.RemoveByID(p.ID())
		}
	}
	r.pruneKept = append(r.pruneKept, breakages...)
}

// PruneWarnings returns the dependency breakages accumulated by the prune-older
// path. kept lists versions retained because a dependent still needs that
// specific version (the default behavior); broken lists versions dropped
// despite a dependent because PruneBreakDeps was set. Both are advisory and
// meant to be surfaced to the operator.
func (r *Repo) PruneWarnings() (kept, broken []aptdata.Breakage) {
	return r.pruneKept, r.pruneBroken
}

// CheckDependencies reports intra-repository dependencies in the current index
// that no package satisfies (see aptdata.CheckDependencies). It is used by the
// verify and rebuild commands to validate that removals and prunes have not
// broken the dependency graph.
func (r *Repo) CheckDependencies() []aptdata.DependencyProblem {
	return aptdata.CheckDependencies(r.idx.Packages())
}

// Remove drops entries matching name (and optional arch/version) from the
// index, returning them. Their files are garbage-collected on Commit once they
// are no longer referenced by any index.
func (r *Repo) Remove(name, arch, version string) []aptdata.Entry {
	return r.idx.Remove(name, arch, version)
}

// nowUnix returns the configured or current unix time.
func (r *Repo) nowUnix() int64 {
	if r.opt.now != 0 {
		return r.opt.now
	}
	return time.Now().Unix()
}

// getAll reads an object fully into memory (indexes are small relative to the
// packages they describe).
func (r *Repo) getAll(ctx context.Context, relpath string) ([]byte, error) {
	rc, err := r.be.Get(ctx, relpath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// sortedMapKeys returns a map's keys in order.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
