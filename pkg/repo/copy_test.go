package repo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/backend"
)

// publishSource builds a small repository to copy from.
func publishSource(t *testing.T, names ...string) string {
	t.Helper()
	ctx := context.Background()
	r, dir := newRepo(t, Options{})
	for _, name := range names {
		var err error
		if strings.HasSuffix(name, ".dsc") {
			_, err = r.AddDSC(ref(t, name))
		} else {
			_, err = r.AddDeb(ref(t, name))
		}
		if err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestObjectsEnumeratesTheWholeSuite(t *testing.T) {
	ctx := context.Background()
	dir := publishSource(t, "hello_2.10-3_all.deb", "foo_1.3.0-1.dsc")

	src := reopen(t, dir, Options{})
	objs, listed, err := src.Objects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !listed {
		t.Error("a local backend reported that it cannot list its contents")
	}

	byKind := map[ObjectKind]int{}
	paths := map[string]bool{}
	for _, o := range objs {
		byKind[o.Kind]++
		paths[o.Path] = true
	}
	if byKind[ObjectRelease] != 1 {
		t.Errorf("found %d Release objects; want 1", byKind[ObjectRelease])
	}
	// The .deb plus the source package's three files.
	if byKind[ObjectPackage] != 4 {
		t.Errorf("found %d package files; want 4", byKind[ObjectPackage])
	}
	if byKind[ObjectConfig] != 0 {
		t.Errorf("found a config object in a repository that has none")
	}
	if !paths["dists/bookworm/Release"] || !paths["pool/main/h/hello/hello_2.10-3_all.deb"] {
		t.Error("the enumeration is missing the Release or a pool file")
	}

	// The Release must sort last so it is the atomic publish point.
	if objs[len(objs)-1].Kind != ObjectRelease {
		t.Errorf("last object is %s; want the Release", objs[len(objs)-1].Kind)
	}
}

func TestCopyExactReplicatesByteForByte(t *testing.T) {
	ctx := context.Background()
	srcDir := publishSource(t, "hello_2.10-3_all.deb", "libfoo_1.3.0-1_amd64.deb")
	dstDir := t.TempDir()

	src := reopen(t, srcDir, Options{})
	objs, _, err := src.Objects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dstBE, err := backend.Open(ctx, dstDir)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := CopyExact(ctx, src.Backend(), dstBE, objs, CopyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != len(objs) || stats.Skipped != 0 {
		t.Errorf("copied %d and skipped %d; want %d and 0", stats.Copied, stats.Skipped, len(objs))
	}

	for _, o := range objs {
		want, err := os.ReadFile(filepath.Join(srcDir, filepath.FromSlash(o.Path)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dstDir, filepath.FromSlash(o.Path)))
		if err != nil {
			t.Errorf("%s was not copied: %v", o.Path, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s differs between source and copy", o.Path)
		}
	}

	// A second run must transfer nothing, which is what makes an interrupted
	// copy resumable. The Release and its signatures are the exception: nothing
	// vouches for their content, so they are always rewritten.
	stats, err = CopyExact(ctx, src.Backend(), dstBE, objs, CopyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped != len(objs)-1 {
		t.Errorf("a repeated copy skipped %d of %d objects; want all but the Release",
			stats.Skipped, len(objs))
	}
}

func TestCopyExactRejectsCorruptedSourceContent(t *testing.T) {
	ctx := context.Background()
	srcDir := publishSource(t, "hello_2.10-3_all.deb")
	src := reopen(t, srcDir, Options{})
	objs, _, err := src.Objects(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt the package after the metadata was written. A copy must refuse to
	// carry a file that does not match what the indexes say it is.
	pkgPath := filepath.Join(srcDir, "pool", "main", "h", "hello", "hello_2.10-3_all.deb")
	if err := os.WriteFile(pkgPath, []byte("not a package"), 0o644); err != nil {
		t.Fatal(err)
	}

	dstBE, err := backend.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CopyExact(ctx, src.Backend(), dstBE, objs, CopyOptions{}); err == nil {
		t.Fatal("a corrupted source file was copied without complaint")
	}
}

func TestCopyEntriesRebuildsTheDestination(t *testing.T) {
	ctx := context.Background()
	srcDir := publishSource(t,
		"hello_2.10-3_all.deb", "libfoo_1.3.0-1_amd64.deb", "libfoo_1.4.0-1_amd64.deb")

	src := reopen(t, srcDir, Options{})
	selected, err := aptdata.Filter{LatestOnly: true}.Apply(src.Index().Entries())
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 {
		t.Fatalf("selected %d entries; want 2", len(selected))
	}

	dstDir := t.TempDir()
	dst, err := Open(ctx, dstDir, Options{Suite: "trixie", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	stats, err := dst.CopyEntriesFrom(ctx, src.Backend(), selected, EntryCopyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 2 {
		t.Errorf("copied %d files; want 2", stats.Copied)
	}
	if _, err := dst.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	back, err := Open(ctx, dstDir, Options{Suite: "trixie"})
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if got := back.Index().Len(); got != 2 {
		t.Errorf("the copy holds %d packages; want 2", got)
	}
	if back.Index().Find("libfoo", "amd64", "1.3.0-1") != nil {
		t.Error("--latest-only still copied the superseded version")
	}
	if _, err := os.Stat(filepath.Join(dstDir, "dists", "trixie", "Release")); err != nil {
		t.Errorf("the copy was not published into the requested suite: %v", err)
	}
}

func TestCopyEntriesIntoAnotherComponentRelocates(t *testing.T) {
	ctx := context.Background()
	srcDir := publishSource(t, "hello_2.10-3_all.deb")
	src := reopen(t, srcDir, Options{})

	dstDir := t.TempDir()
	dst, err := Open(ctx, dstDir, Options{Suite: "bookworm", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	if _, err := dst.CopyEntriesFrom(ctx, src.Backend(), src.Index().Entries(),
		EntryCopyOptions{Component: "contrib"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dstDir, "pool", "contrib", "h", "hello", "hello_2.10-3_all.deb")); err != nil {
		t.Errorf("the package did not move into the new component's pool: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "dists", "bookworm", "contrib", "binary-all", "Packages")); err != nil {
		// arch:all is the only architecture here, so it gets its own index.
		t.Errorf("no index was written for the new component: %v", err)
	}
}

func TestSameRepository(t *testing.T) {
	srcDir := publishSource(t, "hello_2.10-3_all.deb", "libfoo_1.3.0-1_amd64.deb")
	src := reopen(t, srcDir, Options{})

	// An empty destination is always a valid partial copy.
	if err := SameRepository(src.Index(), aptdata.NewIndex("bookworm")); err != nil {
		t.Errorf("an empty destination was rejected: %v", err)
	}

	// A destination holding a subset qualifies.
	partial := aptdata.NewIndex("bookworm")
	partial.Add(src.Index().Packages()[0])
	if err := SameRepository(src.Index(), partial); err != nil {
		t.Errorf("a partial copy was rejected: %v", err)
	}

	// One holding something the source does not have is a different repository.
	otherDir := publishSource(t, "pinned_1.0-1_amd64.deb")
	other := reopen(t, otherDir, Options{})
	err := SameRepository(src.Index(), other.Index())
	if err == nil {
		t.Fatal("an unrelated repository was accepted as a partial copy")
	}
	if !strings.Contains(err.Error(), "pinned") {
		t.Errorf("the error does not name the offending package: %v", err)
	}
}
