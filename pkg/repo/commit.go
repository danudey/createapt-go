package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/backend"
)

// ReasonCopied is the UploadAction reason recorded for a file the copy command
// already transferred directly, outside the staged-upload path. Callers that
// report a plan can recognize it to avoid listing those packages twice.
const ReasonCopied = "copied from the source repository"

// UploadAction describes what will happen (or happened) to one package file.
type UploadAction struct {
	Location string // destination path, relative to the repository root
	Local    string // source path
	Size     int64
	Upload   bool   // true if the file is/was transferred
	Reason   string // human-readable explanation
}

// CopyAction describes a server-side relocation: an identical copy already
// present in the repository at From is duplicated to To (instead of
// re-uploading it), and From is removed during garbage collection.
type CopyAction struct {
	From string
	To   string
	Size int64
}

// Plan summarizes a commit: the metadata that will be written and the set of
// file transfers, relocations, skips and deletions.
type Plan struct {
	Packages      int            // binary packages indexed
	Sources       int            // source packages indexed
	Uploads       []UploadAction // files that need transferring
	Skipped       []UploadAction // files already present (validated remotely)
	Copies        []CopyAction   // files relocated server-side (no re-upload)
	DeletedFiles  []string       // package files no longer referenced, to delete
	MetadataFiles []string       // new index files to write
	ObsoleteMeta  []string       // superseded index files to delete
	BytesToUpload int64          // package bytes (skips + relocations excluded)
	Signed        bool

	// UnreferencedFiles and StaleMetadata are populated only when the
	// corresponding Options are set (rebuild --remove-*). They list files found
	// in the backend that the published state no longer references and that will
	// be deleted during garbage collection.
	UnreferencedFiles []string
	StaleMetadata     []string
}

// metadataSet holds the generated index documents and the Release that covers
// them, each already keyed by the repo-root-relative path it is written to.
type metadataSet struct {
	// files maps a destination path to its bytes. It holds every index, in
	// every compression form, plus the by-hash copies when ByHash is on.
	files map[string][]byte
	// release, releaseSig and inRelease are written last, after every file the
	// Release names is in place.
	release    []byte
	releaseSig []byte
	inRelease  []byte
	// referenced is every path the new Release accounts for, used to decide
	// which previously-published metadata is now obsolete.
	referenced map[string]bool
}

// Commit publishes the current index. It uploads only package files that are
// missing or differ, writes the regenerated indexes, swaps the Release last (so
// clients never see a torn repository), then deletes superseded metadata and
// any package file the new indexes no longer reference. When DryRun is set it
// returns the Plan without transferring anything.
//
// The publish is near-atomic because every index is also written under its own
// checksum (Acquire-By-Hash). A client that read the previous Release keeps
// fetching the indexes that Release named, at by-hash paths this publish does
// not reuse, so it never reads an index that disagrees with its Release. Those
// superseded by-hash copies are deleted at the end of the publish, so a client
// that waits long enough between the two fetches gets a 404 — which apt retries
// — rather than a hash mismatch, which it cannot recover from.
func (r *Repo) Commit(ctx context.Context) (*Plan, error) {
	plan, err := r.buildPlan(ctx)
	if err != nil {
		return nil, err
	}

	meta, err := r.generateMetadata()
	if err != nil {
		return nil, err
	}
	for dest := range meta.files {
		plan.MetadataFiles = append(plan.MetadataFiles, dest)
	}
	sort.Strings(plan.MetadataFiles)
	plan.ObsoleteMeta = r.obsoleteMetadata(meta)
	plan.Signed = meta.inRelease != nil

	if err := r.computeCleanups(ctx, plan, meta); err != nil {
		return nil, err
	}

	if r.opt.DryRun {
		return plan, nil
	}

	// Everything from here on transfers or deletes something, which is what the
	// progress display measures.
	items, bytes := publishWork(plan, meta)
	r.opt.Progress.Start("publishing", items, bytes)
	defer r.opt.Progress.Finish()

	// 1. Relocate identical files server-side (write the new copy now; the old
	// copy is removed during GC, after the Release swap, so the repository is
	// never torn).
	for _, c := range plan.Copies {
		if err := r.copyFile(ctx, c.From, c.To); err != nil {
			return nil, fmt.Errorf("relocate %s -> %s: %w", c.From, c.To, err)
		}
		r.opt.Progress.Item()
	}

	// 2. Upload package files that need transferring. They must be in place
	// before any index names them.
	for _, a := range plan.Uploads {
		if err := r.uploadFile(ctx, a.Location, a.Local); err != nil {
			return nil, fmt.Errorf("upload %s: %w", a.Location, err)
		}
	}

	// 3. Write the indexes, by-hash copies first so a Release that names them
	// is never published ahead of the file it points at.
	for _, dest := range byHashFirst(plan.MetadataFiles) {
		if err := r.putBytes(ctx, dest, meta.files[dest]); err != nil {
			return nil, fmt.Errorf("write index %s: %w", dest, err)
		}
	}

	// 4. Swap the Release last — this is the atomic publish point.
	suite := r.opt.suite()
	if err := r.putBytes(ctx, aptdata.ReleasePath(suite), meta.release); err != nil {
		return nil, fmt.Errorf("write Release: %w", err)
	}
	if meta.releaseSig != nil {
		if err := r.putBytes(ctx, aptdata.ReleaseGPGPath(suite), meta.releaseSig); err != nil {
			return nil, fmt.Errorf("write Release.gpg: %w", err)
		}
	}
	// InRelease carries its signature inside the document, so it is never
	// internally inconsistent. It is written last because it is the file apt
	// prefers, making it the point at which modern clients see the new suite.
	if meta.inRelease != nil {
		if err := r.putBytes(ctx, aptdata.InReleasePath(suite), meta.inRelease); err != nil {
			return nil, fmt.Errorf("write InRelease: %w", err)
		}
	} else if r.old != nil {
		// The suite was signed before and is not now; leaving the old
		// signatures behind would make apt reject the new, valid Release.
		for _, p := range []string{aptdata.InReleasePath(suite), aptdata.ReleaseGPGPath(suite)} {
			if err := r.be.Delete(ctx, p); err != nil {
				return nil, fmt.Errorf("delete stale signature %s: %w", p, err)
			}
		}
	}

	// 5. Garbage-collect superseded metadata and files no longer referenced
	// (including the sources of any relocations performed in step 1).
	for _, groups := range [][]string{plan.ObsoleteMeta, plan.DeletedFiles, plan.UnreferencedFiles, plan.StaleMetadata} {
		for _, p := range groups {
			if err := r.be.Delete(ctx, p); err != nil {
				return nil, fmt.Errorf("delete %s: %w", p, err)
			}
			r.opt.Progress.Item()
		}
	}
	return plan, nil
}

// publishWork counts what Commit is about to do, for the overall progress bar:
// every file it writes, relocates or deletes, and the bytes the writes amount
// to. A relocation and a deletion move no bytes through this process, so they
// count as items only — which is also why the bar is not purely byte-driven.
func publishWork(plan *Plan, meta *metadataSet) (items int, bytes int64) {
	items = len(plan.Copies) + len(plan.Uploads) +
		len(plan.ObsoleteMeta) + len(plan.DeletedFiles) +
		len(plan.UnreferencedFiles) + len(plan.StaleMetadata)
	bytes = plan.BytesToUpload

	for _, body := range meta.files {
		items++
		bytes += int64(len(body))
	}
	for _, body := range [][]byte{meta.release, meta.releaseSig, meta.inRelease} {
		if body != nil {
			items++
			bytes += int64(len(body))
		}
	}
	return items, bytes
}

// byHashFirst orders index writes so a checksum-named copy is always in place
// before the plain path that shares its content.
func byHashFirst(paths []string) []string {
	out := make([]string, 0, len(paths))
	var plain []string
	for _, p := range paths {
		if strings.Contains(p, "/"+aptdata.ByHashDir+"/") {
			out = append(out, p)
		} else {
			plain = append(plain, p)
		}
	}
	return append(out, plain...)
}

// computeCleanups populates plan.UnreferencedFiles and plan.StaleMetadata by
// listing the backend and diffing against the published state. It is a no-op
// unless a RemoveUnreferencedPackages/RemoveStaleMetadata option is set, and
// errors if the backend cannot enumerate its objects.
func (r *Repo) computeCleanups(ctx context.Context, plan *Plan, meta *metadataSet) error {
	if !r.opt.RemoveUnreferencedPackages && !r.opt.RemoveStaleMetadata {
		return nil
	}
	lister, ok := r.be.(backend.Lister)
	if !ok {
		return fmt.Errorf("backend %s cannot list its contents; --remove-unreferenced-packages/--remove-stale-metadata are unavailable here", r.be)
	}

	if r.opt.RemoveUnreferencedPackages {
		referenced := r.idx.Locations()
		objs, err := lister.List(ctx, aptdata.PoolDir+"/")
		if err != nil {
			return fmt.Errorf("list pool: %w", err)
		}
		deleting := toSet(plan.DeletedFiles) // orphans already scheduled
		for _, o := range objs {
			if _, isReferenced := referenced[o.Path]; isReferenced || deleting[o.Path] {
				continue
			}
			plan.UnreferencedFiles = append(plan.UnreferencedFiles, o.Path)
		}
		sort.Strings(plan.UnreferencedFiles)
	}

	if r.opt.RemoveStaleMetadata {
		suite := r.opt.suite()
		keep := map[string]bool{
			aptdata.ReleasePath(suite):    true,
			aptdata.InReleasePath(suite):  true,
			aptdata.ReleaseGPGPath(suite): true,
		}
		for dest := range meta.files {
			keep[dest] = true
		}
		obsolete := toSet(plan.ObsoleteMeta) // deleted anyway; avoid double-listing
		objs, err := lister.List(ctx, aptdata.SuiteDir(suite)+"/")
		if err != nil {
			return fmt.Errorf("list %s: %w", aptdata.SuiteDir(suite), err)
		}
		for _, o := range objs {
			if keep[o.Path] || obsolete[o.Path] {
				continue
			}
			plan.StaleMetadata = append(plan.StaleMetadata, o.Path)
		}
		sort.Strings(plan.StaleMetadata)
	}
	return nil
}

func toSet(items []string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, it := range items {
		s[it] = true
	}
	return s
}

// buildPlan decides, for each staged file, whether it must be uploaded,
// relocated from an existing copy, or skipped, and which now-unreferenced files
// must be garbage-collected. It uses remote validation (RemoteHasher / size) to
// avoid re-uploading files that are already present, and never downloads one.
func (r *Repo) buildPlan(ctx context.Context) (*Plan, error) {
	plan := &Plan{Packages: r.idx.Len(), Sources: r.idx.SourceLen()}

	// Locations the freshly-generated indexes reference, and the size and
	// checksum each is expected to have.
	referenced := r.fileExpectations()

	// orphans: files referenced by the indexes as loaded but no longer
	// referenced now. They are garbage-collected after the Release swap; an
	// orphan whose content matches a staged upload can serve as the source of a
	// server-side relocation instead of a re-upload.
	orphans := map[string]string{} // path -> original entry id
	orphanByID := map[string]string{}
	for p, id := range r.original {
		if _, stillReferenced := referenced[p]; stillReferenced {
			continue
		}
		orphans[p] = id
		if _, ok := orphanByID[id]; !ok {
			orphanByID[id] = p
		}
	}

	_, canCopy := r.be.(backend.Copier)
	hasher, _ := r.be.(backend.RemoteHasher)

	// Deterministic order for staged uploads.
	dests := make([]string, 0, len(r.pending))
	for dest := range r.pending {
		dests = append(dests, dest)
	}
	sort.Strings(dests)

	for _, dest := range dests {
		local := r.pending[dest]
		want := referenced[dest]
		action := UploadAction{Location: dest, Local: local, Size: want.size}

		fi, err := r.be.Stat(ctx, dest)
		missing := false
		switch {
		case errors.Is(err, backend.ErrNotExist):
			missing = true
			action.Upload = true
			action.Reason = "not present"
		case err != nil:
			return nil, fmt.Errorf("stat %s: %w", dest, err)
		default:
			action.Upload, action.Reason, err = r.resolveExisting(ctx, dest, want, fi, hasher)
			if err != nil {
				return nil, err
			}
		}

		// If the file is missing but an identical copy already exists in the
		// repository at a now-unreferenced location, relocate it server-side
		// instead of re-uploading. --force always re-uploads from local (it
		// deliberately does not trust the remote copy).
		if missing && !r.opt.Force && canCopy && want.id != "" {
			if from, ok := orphanByID[want.id]; ok {
				plan.Copies = append(plan.Copies, CopyAction{From: from, To: dest, Size: want.size})
				continue
			}
		}

		if action.Upload {
			plan.Uploads = append(plan.Uploads, action)
			plan.BytesToUpload += action.Size
		} else {
			plan.Skipped = append(plan.Skipped, action)
		}
	}

	// Relocated existing files: referenced by the new indexes, not staged as a
	// local upload, and at a location the loaded indexes did not have (their
	// location changed, e.g. rebuild --pool-layout). Their content already
	// lives in the repository at a now-orphaned location, so move it
	// server-side instead of re-uploading. This pass is a no-op for
	// add/remove/create, where every referenced location is either pending or
	// unchanged.
	for _, dest := range sortedMapKeys(referenced) {
		want := referenced[dest]
		if _, isPending := r.pending[dest]; isPending {
			continue
		}
		if _, wasOriginal := r.original[dest]; wasOriginal {
			continue
		}
		// Already transferred out of band by the copy command (or, under
		// --dry-run, accounted for as if it had been).
		if size, wasCopied := r.copied[dest]; wasCopied {
			plan.Skipped = append(plan.Skipped, UploadAction{Location: dest, Size: size, Reason: ReasonCopied})
			continue
		}
		// A new location for existing content. If it is already present (e.g. a
		// resumed rebuild), trust it; otherwise relocate from the orphan copy.
		if _, err := r.be.Stat(ctx, dest); err == nil {
			plan.Skipped = append(plan.Skipped, UploadAction{Location: dest, Size: want.size, Reason: "present at new location"})
			continue
		} else if !errors.Is(err, backend.ErrNotExist) {
			return nil, fmt.Errorf("stat %s: %w", dest, err)
		}
		if canCopy {
			if from, ok := orphanByID[want.id]; ok {
				plan.Copies = append(plan.Copies, CopyAction{From: from, To: dest, Size: want.size})
				continue
			}
		}
		return nil, fmt.Errorf("cannot place %s: no local copy staged and the backend cannot move it server-side", dest)
	}

	// Everything still unreferenced is garbage. A relocation source is included
	// here too: its copy is written before the Release swap, and the source is
	// removed afterwards.
	for p := range orphans {
		plan.DeletedFiles = append(plan.DeletedFiles, p)
	}
	sort.Strings(plan.DeletedFiles)
	return plan, nil
}

// fileExpectation is what the new indexes say about one published file.
type fileExpectation struct {
	size int64
	// hashes are the digests the index records; sha256 is always present for a
	// .deb and for every file of a source package this tool indexed.
	hashes aptdata.Hashes
	// id is the owning entry's checksum, used to match a relocation source.
	id string
}

// fileExpectations maps every file the current index references to its expected
// size and digests.
func (r *Repo) fileExpectations() map[string]fileExpectation {
	out := map[string]fileExpectation{}
	for _, p := range r.idx.Packages() {
		out[p.Location()] = fileExpectation{size: p.Size(), hashes: p.Checksums(), id: p.ID()}
	}
	for _, s := range r.idx.Sources() {
		dir := s.Directory()
		for _, f := range s.SourceFiles() {
			h := aptdata.Hashes{}
			for algo, v := range map[string]string{
				aptdata.HashMD5: f.MD5, aptdata.HashSHA1: f.SHA1, aptdata.HashSHA256: f.SHA256,
			} {
				if v != "" {
					h[algo] = v
				}
			}
			// The owning entry is the source package, so a relocated source
			// package's files are matched as a unit.
			out[f.Location(dir)] = fileExpectation{size: f.Size, hashes: h, id: f.SHA256}
		}
	}
	return out
}

// resolveExisting decides whether an already-present file must be re-uploaded.
func (r *Repo) resolveExisting(ctx context.Context, dest string, want fileExpectation, fi *backend.FileInfo, hasher backend.RemoteHasher) (upload bool, reason string, err error) {
	// --force means "upload regardless": overwrite the remote file even if it
	// looks identical. This is the only correct behavior when we cannot prove
	// the remote file matches.
	if r.opt.Force {
		return true, "overwriting (--force)", nil
	}
	expect := want.hashes[aptdata.HashSHA256]
	if hasher != nil && expect != "" {
		sum, ok, herr := hasher.Hash(ctx, dest, backend.AlgoSHA256)
		if herr != nil && !errors.Is(herr, backend.ErrNotExist) {
			return false, "", fmt.Errorf("remote hash %s: %w", dest, herr)
		}
		if ok {
			if strings.EqualFold(sum, expect) {
				return false, "present, checksum verified remotely", nil
			}
			return false, "", fmt.Errorf("%s already exists with different content (checksum %s != %s); use --force to overwrite", dest, sum, expect)
		}
	}
	// Fall back to a size comparison when no checksum is available.
	if want.size == 0 || fi.Size == want.size {
		return false, "present, size matches (checksum not verified)", nil
	}
	return false, "", fmt.Errorf("%s already exists with different size (%d != %d); use --force to overwrite", dest, fi.Size, want.size)
}

// indexDoc is one generated index before compression.
type indexDoc struct {
	// dir is the index's directory, relative to the repository root.
	dir string
	// base is the uncompressed filename ("Packages" or "Sources").
	base string
	// body is the rendered document.
	body []byte
}

// generateMetadata renders every index the suite needs, compresses each into
// the configured forms, and builds the Release that covers them.
func (r *Repo) generateMetadata() (*metadataSet, error) {
	suite := r.opt.suite()
	suiteDir := aptdata.SuiteDir(suite)
	algos := r.opt.hashes()

	docs, components, arches := r.indexDocuments()

	meta := &metadataSet{files: map[string][]byte{}, referenced: map[string]bool{}}
	rel := r.buildRelease(components, arches)

	// publish records one file the Release will cover: the file itself, its
	// entry in the Release's checksum blocks, and — when Acquire-By-Hash is on
	// — a copy of it under each of those checksums. Everything the Release
	// names goes through here, because a Release that advertises by-hash
	// promises a by-hash copy of every file it lists, not only of the package
	// indexes.
	publish := func(dest string, body []byte) {
		hashes := aptdata.HashBytes(body, algos)
		meta.files[dest] = body
		meta.referenced[dest] = true
		rel.Files = append(rel.Files, aptdata.IndexFile{
			Path:   mustRel(suiteDir, dest),
			Size:   int64(len(body)),
			Hashes: hashes,
		})
		if !r.opt.byHash() {
			return
		}
		for _, algo := range algos {
			byHash := aptdata.ByHashPath(path.Dir(dest), algo, hashes[algo])
			meta.files[byHash] = body
			meta.referenced[byHash] = true
		}
	}

	for _, doc := range docs {
		for _, comp := range r.opt.compressions() {
			body, err := comp.Compress(doc.body)
			if err != nil {
				return nil, fmt.Errorf("compress %s: %w", path.Join(doc.dir, doc.base), err)
			}
			publish(path.Join(doc.dir, doc.base+comp.Ext()), body)
		}
	}

	// The per-component Release files carry no checksums of their own, so they
	// are written uncompressed and listed like any other index.
	for _, cr := range r.componentReleases(rel, components, arches) {
		publish(cr.dest, cr.body)
	}

	meta.release = rel.Render()
	meta.referenced[aptdata.ReleasePath(suite)] = true

	if r.opt.Signer != nil {
		sig, err := r.opt.Signer.SignDetached(meta.release)
		if err != nil {
			return nil, fmt.Errorf("sign Release: %w", err)
		}
		inline, err := r.opt.Signer.SignClearsigned(meta.release)
		if err != nil {
			return nil, fmt.Errorf("clearsign Release: %w", err)
		}
		meta.releaseSig = sig
		meta.inRelease = inline
		meta.referenced[aptdata.ReleaseGPGPath(suite)] = true
		meta.referenced[aptdata.InReleasePath(suite)] = true
	}
	return meta, nil
}

// indexDocuments renders one Packages document per component and architecture,
// and one Sources document per component that has source packages. It returns
// the documents along with the components and architectures they cover.
func (r *Repo) indexDocuments() (docs []indexDoc, components, arches []string) {
	suite := r.opt.suite()
	components = r.idx.Components()
	if len(components) == 0 {
		// A suite being created empty still declares the component it will
		// receive packages into, so a client can subscribe to it now.
		components = []string{r.opt.component()}
	}
	arches = r.architectures()

	byComponent := map[string][]*aptdata.Package{}
	for _, p := range r.idx.Packages() {
		byComponent[p.Component] = append(byComponent[p.Component], p)
	}
	srcByComponent := map[string][]*aptdata.Source{}
	for _, s := range r.idx.Sources() {
		srcByComponent[s.Component] = append(srcByComponent[s.Component], s)
	}

	for _, component := range components {
		for _, arch := range arches {
			var selected []*aptdata.Package
			for _, p := range byComponent[component] {
				// An arch:all package belongs in every architecture's index,
				// because apt only reads the indexes for the architectures the
				// client is configured for. Writing it into each one is what
				// every apt version understands, at the cost of repeating a
				// stanza that is usually a few hundred bytes.
				if p.Arch() == arch || p.Arch() == aptdata.ArchAll {
					selected = append(selected, p)
				}
			}
			docs = append(docs, indexDoc{
				dir:  aptdata.BinaryIndexDir(suite, component, arch),
				base: "Packages",
				body: aptdata.WritePackages(selected),
			})
		}
		if srcs := srcByComponent[component]; len(srcs) > 0 {
			docs = append(docs, indexDoc{
				dir:  aptdata.SourceIndexDir(suite, component),
				base: "Sources",
				body: aptdata.WriteSources(srcs),
			})
		}
	}
	return docs, components, arches
}

// architectures decides which binary-<arch> indexes the suite publishes.
//
// An explicit Architectures option wins outright, so an operator can retire an
// architecture. Otherwise the set is the architectures actually indexed plus
// any the previous Release listed: an architecture must not vanish because its
// last package was removed, since a client subscribed to it would then get no
// index at all, which apt reports as a broken repository rather than as an
// empty one.
//
// A suite with nothing in it still needs one index, or there is nothing for a
// client to subscribe to. "all" is used for that, and for the case where every
// package is Architecture: all — apt 1.1 and later read a binary-all index, and
// there is no alternative an older apt could use either.
func (r *Repo) architectures() []string {
	if len(r.opt.Architectures) > 0 {
		return append([]string(nil), r.opt.Architectures...)
	}

	seen := map[string]bool{}
	for _, a := range r.idx.Architectures() {
		seen[a] = true
	}
	if r.old != nil {
		for _, a := range r.old.Architectures() {
			if a != "" {
				seen[a] = true
			}
		}
	}
	if len(seen) == 0 {
		return []string{aptdata.ArchAll}
	}
	// "all" is only ever published as an architecture in its own right when it
	// is the only one; alongside a real architecture, arch:all packages are
	// written into that architecture's index instead.
	if len(seen) > 1 {
		delete(seen, aptdata.ArchAll)
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// componentRelease is one generated per-component, per-architecture Release.
type componentRelease struct {
	dest string
	body []byte
}

func (r *Repo) componentReleases(rel *aptdata.Release, components, arches []string) []componentRelease {
	suite := r.opt.suite()
	var out []componentRelease
	for _, component := range components {
		for _, arch := range arches {
			out = append(out, componentRelease{
				dest: path.Join(aptdata.BinaryIndexDir(suite, component, arch), "Release"),
				body: aptdata.ComponentRelease(rel, component, arch),
			})
		}
		if r.componentHasSources(component) {
			out = append(out, componentRelease{
				dest: path.Join(aptdata.SourceIndexDir(suite, component), "Release"),
				body: aptdata.ComponentRelease(rel, component, aptdata.ArchSource),
			})
		}
	}
	return out
}

func (r *Repo) componentHasSources(component string) bool {
	for _, s := range r.idx.Sources() {
		if s.Component == component {
			return true
		}
	}
	return false
}

// buildRelease assembles the suite's Release file, preserving any descriptive
// field a previous publish recorded that this invocation does not set.
func (r *Repo) buildRelease(components, arches []string) *aptdata.Release {
	rel := aptdata.NewRelease()
	if r.idx.Release != nil {
		rel.Para = r.idx.Release.Clone()
	}

	set := func(field, value string) {
		if value != "" {
			rel.Set(field, value)
		}
	}
	rel.Set(aptdata.FieldSuite, r.opt.suite())
	set(aptdata.FieldOrigin, r.opt.Origin)
	set(aptdata.FieldLabel, r.opt.Label)
	set(aptdata.FieldCodename, r.opt.Codename)
	set(aptdata.FieldReleaseVer, r.opt.ReleaseVersion)
	set(aptdata.FieldReleaseDesc, r.opt.Description)

	// Architectures lists only the indexes that exist. "all" is deliberately
	// absent unless it is the only architecture: arch:all packages are written
	// into every architecture's index instead, which is what apt of every
	// vintage expects, and listing an architecture with no index would make
	// apt fail on a file that is not there.
	rel.Set(aptdata.FieldArchitectures, strings.Join(arches, " "))
	rel.Set(aptdata.FieldComponents, strings.Join(components, " "))

	if r.opt.byHash() {
		rel.Set(aptdata.FieldAcquireByHash, "yes")
	} else {
		rel.Para.Delete(aptdata.FieldAcquireByHash)
	}
	rel.SetDate(time.Unix(r.nowUnix(), 0).UTC(), r.opt.Validity)
	return rel
}

// obsoleteMetadata returns previously-published metadata files that the new
// Release replaces. It covers both a plain index that no longer exists (an
// architecture or component that has gone away) and every by-hash copy the new
// Release does not name.
func (r *Repo) obsoleteMetadata(meta *metadataSet) []string {
	if r.old == nil {
		return nil
	}
	suiteDir := aptdata.SuiteDir(r.opt.suite())
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if meta.referenced[p] || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}

	for _, f := range r.old.Files {
		dest := path.Join(suiteDir, f.Path)
		add(dest)
		for algo, digest := range f.Hashes {
			add(aptdata.ByHashPath(path.Dir(dest), algo, digest))
		}
	}
	sort.Strings(out)
	return out
}

// mustRel returns dest relative to base. Both are constructed from the same
// suite directory, so the prefix always matches.
func mustRel(base, dest string) string {
	return strings.TrimPrefix(strings.TrimPrefix(dest, base), "/")
}

func (r *Repo) uploadFile(ctx context.Context, dest, local string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	task := r.opt.Progress.Task(dest, fi.Size())
	defer task.Done()
	return r.be.Put(ctx, dest, task.Reader(f), fi.Size())
}

func (r *Repo) putBytes(ctx context.Context, dest string, data []byte) error {
	task := r.opt.Progress.Task(dest, int64(len(data)))
	defer task.Done()
	return r.be.Put(ctx, dest, task.Reader(bytesReader(data)), int64(len(data)))
}

// copyFile relocates an existing file within the backend without transferring
// its bytes. It is only called for actions the plan produced, which requires
// the backend to be a Copier.
func (r *Repo) copyFile(ctx context.Context, from, to string) error {
	c, ok := r.be.(backend.Copier)
	if !ok {
		return fmt.Errorf("backend does not support server-side copy")
	}
	return c.Copy(ctx, from, to)
}
