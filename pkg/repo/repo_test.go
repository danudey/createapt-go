package repo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createapt-go/pkg/aptdata"
)

// referencePackages locates the sample packages built by reference/gen.sh.
func referencePackages(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "reference", "packages"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("reference packages are not built; run reference/gen.sh (%v)", err)
	}
	return dir
}

func ref(t *testing.T, name string) string {
	return filepath.Join(referencePackages(t), name)
}

// newRepo opens an empty repository on a fresh temporary directory.
func newRepo(t *testing.T, opts Options) (*Repo, string) {
	t.Helper()
	dir := t.TempDir()
	opts.Create = true
	if opts.Suite == "" {
		opts.Suite = "bookworm"
	}
	r, err := Open(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, dir
}

// reopen loads the repository back from disk, which is the only way to prove
// what was actually published rather than what was held in memory.
func reopen(t *testing.T, dir string, opts Options) *Repo {
	t.Helper()
	if opts.Suite == "" {
		opts.Suite = "bookworm"
	}
	r, err := Open(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestAddAndCommitPublishesAReadableSuite(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t, Options{})

	for _, name := range []string{"hello_2.10-3_all.deb", "libfoo_1.3.0-1_amd64.deb"} {
		if _, err := r.AddDeb(ref(t, name)); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	plan, err := r.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Packages != 2 || len(plan.Uploads) != 2 {
		t.Errorf("plan = %d package(s), %d upload(s); want 2 and 2", plan.Packages, len(plan.Uploads))
	}

	// The published files must be where apt expects them.
	for _, want := range []string{
		"dists/bookworm/Release",
		"dists/bookworm/main/binary-amd64/Packages",
		"dists/bookworm/main/binary-amd64/Packages.gz",
		"pool/main/h/hello/hello_2.10-3_all.deb",
		"pool/main/f/foo/libfoo_1.3.0-1_amd64.deb",
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(want))); err != nil {
			t.Errorf("%s was not published: %v", want, err)
		}
	}

	// Reading the repository back must find both packages, including the
	// arch:all one, which is written into every architecture's index.
	back := reopen(t, dir, Options{})
	if got := back.Index().Len(); got != 2 {
		t.Errorf("reopened index holds %d packages; want 2", got)
	}
	if back.Index().Find("hello", aptdata.ArchAll, "2.10-3") == nil {
		t.Error("the arch:all package did not survive a publish/reload round trip")
	}
}

func TestReleaseListsOnlyRealArchitectures(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t, Options{})
	for _, name := range []string{"hello_2.10-3_all.deb", "libfoo_1.3.0-1_amd64.deb"} {
		if _, err := r.AddDeb(ref(t, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "dists", "bookworm", "Release"))
	if err != nil {
		t.Fatal(err)
	}
	rel, err := aptdata.ParseRelease(data)
	if err != nil {
		t.Fatal(err)
	}
	// "all" must not be listed: arch:all packages go into every real
	// architecture's index, and listing an architecture with no index of its
	// own would make apt fail on a file that is not there.
	if got := strings.Join(rel.Architectures(), ","); got != "amd64" {
		t.Errorf("Architectures = %q; want amd64 alone", got)
	}
	if !rel.AcquireByHash() {
		t.Error("Acquire-By-Hash was not advertised")
	}
	// Every file the Release names must exist, including its by-hash copy.
	for _, f := range rel.Files {
		full := filepath.Join(dir, "dists", "bookworm", filepath.FromSlash(f.Path))
		if _, err := os.Stat(full); err != nil {
			t.Errorf("the Release names %s but it is absent: %v", f.Path, err)
		}
		byHash := aptdata.ByHashPath(filepath.Dir(f.Path), aptdata.HashSHA256, f.Hashes[aptdata.HashSHA256])
		if _, err := os.Stat(filepath.Join(dir, "dists", "bookworm", filepath.FromSlash(byHash))); err != nil {
			t.Errorf("no by-hash copy of %s: %v", f.Path, err)
		}
	}
}

// TestRepublishDoesNotReUpload is the minimal-transfer guarantee: adding a
// package that is already published transfers nothing.
func TestRepublishDoesNotReUpload(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t, Options{})
	if _, err := r.AddDeb(ref(t, "hello_2.10-3_all.deb")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	again := reopen(t, dir, Options{})
	if _, err := again.AddDeb(ref(t, "hello_2.10-3_all.deb")); err != nil {
		t.Fatal(err)
	}
	plan, err := again.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Uploads) != 0 || plan.BytesToUpload != 0 {
		t.Errorf("re-adding an identical package transferred %d file(s) (%d bytes); want none",
			len(plan.Uploads), plan.BytesToUpload)
	}
	if len(plan.Skipped) != 1 {
		t.Errorf("got %d skips; want 1", len(plan.Skipped))
	}
	if !strings.Contains(plan.Skipped[0].Reason, "checksum verified") {
		t.Errorf("skip reason = %q; want it to say the checksum was verified", plan.Skipped[0].Reason)
	}
}

func TestSupersedeAndGarbageCollect(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t, Options{})
	if _, err := r.AddDeb(ref(t, "libfoo_1.3.0-1_amd64.deb")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Adding a newer version without --prune-older keeps both.
	again := reopen(t, dir, Options{})
	if _, err := again.AddDeb(ref(t, "libfoo_1.4.0-1_amd64.deb")); err != nil {
		t.Fatal(err)
	}
	if _, err := again.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := reopen(t, dir, Options{}).Index().Len(); got != 2 {
		t.Errorf("index holds %d packages; want both versions", got)
	}

	// With --prune-older the old version goes, and so does its file.
	pruning := reopen(t, dir, Options{PruneOlder: true})
	if _, err := pruning.AddDeb(ref(t, "libfoo_1.4.0-1_amd64.deb")); err != nil {
		t.Fatal(err)
	}
	plan, err := pruning.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.DeletedFiles) != 1 {
		t.Fatalf("plan deletes %v; want the superseded .deb", plan.DeletedFiles)
	}
	old := filepath.Join(dir, "pool", "main", "f", "foo", "libfoo_1.3.0-1_amd64.deb")
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the superseded package file was not garbage-collected (%v)", err)
	}
}

func TestPruneIsBlockedByADependent(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t, Options{})
	for _, name := range []string{"pinned_1.0-1_amd64.deb", "pinner_1.0-1_amd64.deb"} {
		if _, err := r.AddDeb(ref(t, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// pinner depends on pinned (= 1.0-1), so adding pinned 2.0-1 with
	// --prune-older must keep 1.0-1 and say why.
	keeping := reopen(t, dir, Options{PruneOlder: true})
	if _, err := keeping.AddDeb(ref(t, "pinned_2.0-1_amd64.deb")); err != nil {
		t.Fatal(err)
	}
	kept, broken := keeping.PruneWarnings()
	if len(kept) != 1 || len(broken) != 0 {
		t.Fatalf("got %d kept and %d broken warnings; want 1 and 0", len(kept), len(broken))
	}
	if kept[0].Provider.Version() != "1.0-1" || kept[0].Dependent.Name() != "pinner" {
		t.Errorf("warning = %s; want pinned 1.0-1 kept for pinner", kept[0])
	}
	if _, err := keeping.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := reopen(t, dir, Options{}).Index().Len(); got != 3 {
		t.Errorf("index holds %d packages; want 3 (the pinned version was kept)", got)
	}

	// With --prune-break-deps the version goes and the breakage is reported.
	breaking := reopen(t, dir, Options{PruneOlder: true, PruneBreakDeps: true})
	if _, err := breaking.AddDeb(ref(t, "pinned_2.0-1_amd64.deb")); err != nil {
		t.Fatal(err)
	}
	_, broken = breaking.PruneWarnings()
	if len(broken) != 1 {
		t.Fatalf("got %d broken warnings; want 1", len(broken))
	}
	if _, err := breaking.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if problems := reopen(t, dir, Options{}).CheckDependencies(); len(problems) != 1 {
		t.Errorf("got %d dependency problems after the forced prune; want 1", len(problems))
	}
}

func TestRemoveDeletesTheFile(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t, Options{})
	if _, err := r.AddDeb(ref(t, "hello_2.10-3_all.deb")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	removing := reopen(t, dir, Options{})
	if removed := removing.Remove("hello", "", ""); len(removed) != 1 {
		t.Fatalf("removed %d packages; want 1", len(removed))
	}
	if _, err := removing.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pool", "main", "h", "hello", "hello_2.10-3_all.deb")); !os.IsNotExist(err) {
		t.Errorf("the removed package's file is still present (%v)", err)
	}
	if got := reopen(t, dir, Options{}).Index().Len(); got != 0 {
		t.Errorf("index holds %d packages after the removal; want 0", got)
	}
}

func TestSourcePackageRoundTrip(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t, Options{})
	src, err := r.AddDSC(ref(t, "foo_1.3.0-1.dsc"))
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Locations()) != 3 {
		t.Errorf("the source owns %d files; want 3", len(src.Locations()))
	}
	plan, err := r.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Sources != 1 || len(plan.Uploads) != 3 {
		t.Errorf("plan = %d source(s), %d upload(s); want 1 and 3", plan.Sources, len(plan.Uploads))
	}
	if _, err := os.Stat(filepath.Join(dir, "dists", "bookworm", "main", "source", "Sources")); err != nil {
		t.Errorf("no Sources index was published: %v", err)
	}

	back := reopen(t, dir, Options{})
	if back.Index().SourceLen() != 1 {
		t.Fatalf("reopened index holds %d source packages; want 1", back.Index().SourceLen())
	}
	got := back.Index().Sources()[0]
	if got.Name() != "foo" || got.Version() != "1.3.0-1" {
		t.Errorf("source = %s; want foo_1.3.0-1", got.ID3())
	}
}

func TestVerifyDetectsATamperedPackage(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t, Options{})
	if _, err := r.AddDeb(ref(t, "hello_2.10-3_all.deb")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	clean := reopen(t, dir, Options{})
	res, err := clean.Verify(ctx, VerifyOptions{Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("a freshly published repository did not verify: %v", res.Problems)
	}
	if res.ChecksumVerified != 1 {
		t.Errorf("verified %d checksums from content; want 1 (local disk always can)", res.ChecksumVerified)
	}

	// Append a byte to the published package: the size and checksum both stop
	// matching, which is exactly the failure apt reports to users.
	pkgPath := filepath.Join(dir, "pool", "main", "h", "hello", "hello_2.10-3_all.deb")
	f, err := os.OpenFile(pkgPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("x"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	tampered := reopen(t, dir, Options{})
	res, err = tampered.Verify(ctx, VerifyOptions{Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatal("verify passed on a tampered package")
	}
}

func TestRelocateMovesServerSideWithoutReUploading(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t, Options{})
	if _, err := r.AddDeb(ref(t, "hello_2.10-3_all.deb")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Switching to the flat layout changes every package's destination. The
	// content is unchanged, so the local backend renames it rather than
	// anything being uploaded.
	flat := reopen(t, dir, Options{FlatPool: true})
	if moved := flat.RelocateAll(); moved != 1 {
		t.Fatalf("RelocateAll moved %d entries; want 1", moved)
	}
	plan, err := flat.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Uploads) != 0 {
		t.Errorf("a relocation uploaded %d file(s); want none", len(plan.Uploads))
	}
	if len(plan.Copies) != 1 {
		t.Fatalf("got %d server-side copies; want 1", len(plan.Copies))
	}
	if _, err := os.Stat(filepath.Join(dir, "pool", "main", "hello_2.10-3_all.deb")); err != nil {
		t.Errorf("the package is not at its new location: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pool", "main", "h", "hello", "hello_2.10-3_all.deb")); !os.IsNotExist(err) {
		t.Errorf("the old copy was not cleaned up (%v)", err)
	}
}

func TestRefreshFromPackagesCorrectsAStaleIndex(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t, Options{})
	if _, err := r.AddDeb(ref(t, "libfoo_1.4.0-1_amd64.deb")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Publish a different build over the indexed file, as a careless upload
	// would. The index now describes a package that is not there.
	published := filepath.Join(dir, "pool", "main", "f", "foo", "libfoo_1.4.0-1_amd64.deb")
	replacement, err := os.ReadFile(ref(t, "libfoo_1.3.0-1_amd64.deb"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(published, replacement, 0o644); err != nil {
		t.Fatal(err)
	}

	stale := reopen(t, dir, Options{})
	if res, err := stale.Verify(ctx, VerifyOptions{}); err != nil {
		t.Fatal(err)
	} else if res.OK() {
		t.Fatal("verify passed although the published file no longer matches the index")
	}

	fixing := reopen(t, dir, Options{})
	res, err := fixing.RefreshFromPackages(ctx, RefreshOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Read != 1 || res.Changed != 1 {
		t.Errorf("refresh read %d and corrected %d; want 1 and 1", res.Read, res.Changed)
	}
	if _, err := fixing.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	fixed := reopen(t, dir, Options{})
	if got := fixed.Index().Packages()[0].Version(); got != "1.3.0-1" {
		t.Errorf("after the refresh the index says version %q; want the file's own 1.3.0-1", got)
	}
	if res, err := fixed.Verify(ctx, VerifyOptions{}); err != nil {
		t.Fatal(err)
	} else if !res.OK() {
		t.Errorf("verify still fails after a refresh: %v", res.Problems)
	}
}

func TestOpenRefusesAMissingSuiteUnlessCreating(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(context.Background(), dir, Options{Suite: "bookworm"}); err == nil {
		t.Fatal("opening a non-existent suite without Create succeeded")
	} else if !strings.Contains(err.Error(), "bookworm") {
		t.Errorf("the error does not name the suite: %v", err)
	}
}

func TestOpenRejectsAnInvalidSuite(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(context.Background(), dir, Options{Suite: "../escape", Create: true}); err == nil {
		t.Fatal("a suite name that escapes dists/ was accepted")
	}
}

// TestEmptySuiteIsStillUsable is a regression guard. A suite with no packages
// — freshly created, or emptied by removing the last one — must still publish
// an index. Without one, apt finds nothing to fetch and reports the repository
// as broken rather than as empty, which is the worst possible answer for a
// repository that is merely waiting for its first upload.
func TestEmptySuiteIsStillUsable(t *testing.T) {
	ctx := context.Background()

	t.Run("freshly created", func(t *testing.T) {
		r, dir := newRepo(t, Options{})
		if _, err := r.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		assertSuiteHasAnIndex(t, dir, "bookworm")
	})

	t.Run("emptied by removal", func(t *testing.T) {
		r, dir := newRepo(t, Options{})
		if _, err := r.AddDeb(ref(t, "libfoo_1.3.0-1_amd64.deb")); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		emptying := reopen(t, dir, Options{})
		if removed := emptying.Remove("libfoo", "", ""); len(removed) != 1 {
			t.Fatalf("removed %d packages; want 1", len(removed))
		}
		if _, err := emptying.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		// The architecture the package occupied must survive its removal, so a
		// client subscribed to amd64 still finds an index there.
		assertSuiteHasAnIndex(t, dir, "bookworm")
		if _, err := os.Stat(filepath.Join(dir, "dists", "bookworm", "main", "binary-amd64", "Packages")); err != nil {
			t.Errorf("the amd64 index vanished when its last package was removed: %v", err)
		}
	})
}

// assertSuiteHasAnIndex checks that a published suite names at least one
// architecture and that every file its Release lists is present.
func assertSuiteHasAnIndex(t *testing.T, dir, suite string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "dists", suite, "Release"))
	if err != nil {
		t.Fatalf("no Release was published: %v", err)
	}
	rel, err := aptdata.ParseRelease(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.Architectures()) == 0 {
		t.Error("the Release lists no architectures, so a client has nothing to fetch")
	}
	if len(rel.Components()) == 0 {
		t.Error("the Release lists no components")
	}

	var sawPackages bool
	for _, f := range rel.Files {
		if _, err := os.Stat(filepath.Join(dir, "dists", suite, filepath.FromSlash(f.Path))); err != nil {
			t.Errorf("the Release names %s but it is absent: %v", f.Path, err)
		}
		if strings.HasSuffix(f.Path, "/Packages") {
			sawPackages = true
		}
	}
	if !sawPackages {
		t.Error("the Release covers no Packages index")
	}
}

// TestExplicitArchitecturesRetireOne proves the escape hatch: naming the
// architectures drops the ones left out, which is the only way an architecture
// should ever disappear.
func TestExplicitArchitecturesRetireOne(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t, Options{})
	if _, err := r.AddDeb(ref(t, "libfoo_1.3.0-1_amd64.deb")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	retiring := reopen(t, dir, Options{Architectures: []string{"arm64"}})
	if _, err := retiring.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "dists", "bookworm", "Release"))
	if err != nil {
		t.Fatal(err)
	}
	rel, err := aptdata.ParseRelease(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rel.Architectures(), ","); got != "arm64" {
		t.Errorf("Architectures = %q; want the explicitly named arm64 alone", got)
	}
}
