//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnsignedRepositoryIsUsableByApt publishes to local disk and reads the
// result back with a real apt, which is the baseline every other scenario
// builds on.
func TestUnsignedRepositoryIsUsableByApt(t *testing.T) {
	dir := t.TempDir()
	cli(t, "add", dir,
		pkgPath(t, "hello_2.10-3_all.deb"),
		pkgPath(t, "libfoo_1.3.0-1_amd64.deb"),
		"--suite", "bookworm")

	apt := newAptClient(t)
	// An unsigned repository has to be marked trusted, exactly as a user would.
	apt.Source("file://"+dir, "bookworm", "main", "trusted=yes")
	apt.Update()

	if got := apt.Candidate("hello"); got != "2.10-3" {
		t.Errorf("apt's candidate for hello is %q; want 2.10-3", got)
	}
	if got := apt.Candidate("libfoo"); got != "1.3.0-1" {
		t.Errorf("apt's candidate for libfoo is %q; want 1.3.0-1", got)
	}
	// Downloading exercises the size and checksum the Packages index records.
	apt.Download("hello")
}

// TestSignedRepositoryVerifiesAndRejectsWithoutTheKey is the signing contract:
// apt accepts the repository when it trusts the key and refuses it when it does
// not.
func TestSignedRepositoryVerifiesAndRejectsWithoutTheKey(t *testing.T) {
	key := newSigningKey(t)
	dir := t.TempDir()

	cli(t, "add", dir,
		pkgPath(t, "hello_2.10-3_all.deb"),
		pkgPath(t, "libfoo_1.3.0-1_amd64.deb"),
		"--suite", "bookworm",
		"--origin", "Createapt E2E", "--label", "Createapt",
		"--sign-release", "--gpg-key-id", key.UID)

	for _, name := range []string{"InRelease", "Release", "Release.gpg"} {
		if _, err := os.Stat(filepath.Join(dir, "dists", "bookworm", name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}

	// Without the key, apt must refuse the repository rather than use it.
	untrusting := newAptClient(t)
	untrusting.Source("file://"+dir, "bookworm", "main", "")
	untrusting.UpdateExpectingFailure()

	// With the key, everything works and nothing has to be marked trusted.
	apt := newAptClient(t)
	apt.Trust(key.Public)
	apt.Source("file://"+dir, "bookworm", "main", "")
	apt.Update()
	if got := apt.Candidate("hello"); got != "2.10-3" {
		t.Errorf("apt's candidate for hello is %q; want 2.10-3", got)
	}
	apt.Download("libfoo")
}

// TestSourcePackagesAreUsableByApt covers the deb-src half of the repository.
func TestSourcePackagesAreUsableByApt(t *testing.T) {
	dir := t.TempDir()
	cli(t, "add", dir,
		pkgPath(t, "libfoo_1.3.0-1_amd64.deb"),
		pkgPath(t, "foo_1.3.0-1.dsc"),
		"--suite", "bookworm")

	apt := newAptClient(t)
	apt.SourceLine(fmt.Sprintf("deb [trusted=yes] file://%s bookworm main\ndeb-src [trusted=yes] file://%s bookworm main", dir, dir))
	apt.Update()

	if got := apt.SourceCandidate("foo"); got != "1.3.0-1" {
		t.Errorf("apt's source candidate for foo is %q; want 1.3.0-1", got)
	}
}

// TestAddIsIncrementalAndPrunes walks the everyday lifecycle: add, add again
// (transferring nothing), add a newer version, prune, remove.
func TestAddIsIncrementalAndPrunes(t *testing.T) {
	dir := t.TempDir()
	cli(t, "add", dir, pkgPath(t, "libfoo_1.3.0-1_amd64.deb"), "--suite", "bookworm")

	// Re-adding the same package must upload nothing.
	out := cli(t, "add", dir, pkgPath(t, "libfoo_1.3.0-1_amd64.deb"))
	if !strings.Contains(out, "0 upload(s)") {
		t.Errorf("re-adding an identical package transferred something:\n%s", out)
	}
	// The suite recorded in the config must have been reused.
	if strings.Contains(out, "dists/stable/") {
		t.Errorf("the recorded suite was not reused; the run published into stable:\n%s", out)
	}

	// A newer version coexists with the old one by default.
	cli(t, "add", dir, pkgPath(t, "libfoo_1.4.0-1_amd64.deb"))
	if listing := cli(t, "list", dir); !strings.Contains(listing, "2 package(s)") {
		t.Errorf("both versions should be listed:\n%s", listing)
	}

	// --prune-older drops the superseded one and deletes its file.
	cli(t, "add", dir, pkgPath(t, "libfoo_1.4.0-1_amd64.deb"), "--prune-older")
	old := filepath.Join(dir, "pool", "main", "f", "foo", "libfoo_1.3.0-1_amd64.deb")
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the pruned package's file survived (%v)", err)
	}

	apt := newAptClient(t)
	apt.Source("file://"+dir, "bookworm", "main", "trusted=yes")
	apt.Update()
	if got := apt.Candidate("libfoo"); got != "1.4.0-1" {
		t.Errorf("apt's candidate after the prune is %q; want 1.4.0-1", got)
	}

	// Removing the last package leaves an empty but still valid suite.
	cli(t, "remove", dir, "libfoo")
	apt.Update()
	if got := apt.Candidate("libfoo"); got != "" {
		t.Errorf("apt still offers libfoo %q after it was removed", got)
	}
}

// TestPruneRespectsADependent proves the dependency guard is wired through the
// CLI, not only the library.
func TestPruneRespectsADependent(t *testing.T) {
	dir := t.TempDir()
	cli(t, "add", dir,
		pkgPath(t, "pinned_1.0-1_amd64.deb"),
		pkgPath(t, "pinner_1.0-1_amd64.deb"),
		"--suite", "bookworm")

	out := cli(t, "add", dir, pkgPath(t, "pinned_2.0-1_amd64.deb"), "--prune-older")
	if !strings.Contains(out, "keeping pinned_1.0-1_amd64") {
		t.Errorf("the pinned version was not protected:\n%s", out)
	}

	out = cli(t, "add", dir, pkgPath(t, "pinned_2.0-1_amd64.deb"), "--prune-older", "--prune-break-deps")
	if !strings.Contains(out, "--prune-break-deps") {
		t.Errorf("the forced prune did not report the breakage:\n%s", out)
	}
	// verify must now report the dependency the prune broke.
	if _, err := tryCLI(t, "verify", dir); err == nil {
		t.Error("verify passed although the prune left a dependency unsatisfied")
	}
}

// TestVerifyAndCheckDetectARewrittenPackage covers the failure the tool exists
// to catch, through both commands, and the rebuild that repairs it.
func TestVerifyAndCheckDetectARewrittenPackage(t *testing.T) {
	dir := t.TempDir()
	cli(t, "add", dir, pkgPath(t, "libfoo_1.4.0-1_amd64.deb"), "--suite", "bookworm")

	// Publish a different build over the indexed file.
	replacement, err := os.ReadFile(pkgPath(t, "libfoo_1.3.0-1_amd64.deb"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "pool", "main", "f", "foo", "libfoo_1.4.0-1_amd64.deb")
	if err := os.WriteFile(target, replacement, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := tryCLI(t, "verify", dir); err == nil {
		t.Error("verify passed on a repository whose package was rewritten")
	}
	if _, err := tryCLI(t, "check", "--level", "head", "--arch", "any", dir); err == nil {
		t.Error("check passed on a repository whose package was rewritten")
	}

	// apt sees it as the mismatch users report.
	apt := newAptClient(t)
	apt.Source("file://"+dir, "bookworm", "main", "trusted=yes")
	apt.Update()
	out, _ := apt.run("apt-get", "install", "--download-only", "--reinstall", "-y", "libfoo")
	if !strings.Contains(out, "mismatch") {
		t.Logf("apt output while downloading a rewritten package:\n%s", out)
	}

	// rebuild re-reads the file and republishes an index that matches it.
	cli(t, "rebuild", dir, "--yes")
	cli(t, "verify", dir)
	cli(t, "check", "--level", "fetch", "--arch", "any", "--versions", "all", dir)
}

// TestCopyExactProducesAReplica mirrors a signed repository and proves the copy
// is still acceptable to apt under the original signature.
func TestCopyExactProducesAReplica(t *testing.T) {
	key := newSigningKey(t)
	src := t.TempDir()
	cli(t, "add", src,
		pkgPath(t, "hello_2.10-3_all.deb"),
		pkgPath(t, "libfoo_1.3.0-1_amd64.deb"),
		"--suite", "bookworm", "--sign-release", "--gpg-key-id", key.UID)

	dst := t.TempDir()
	cli(t, "copy", src, dst, "--keyring", key.KeyringFile(t))

	// Every file must be byte-identical, apart from the config's copy_source.
	filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "createapt-go.json" {
			return nil
		}
		want, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Errorf("%s was not copied: %v", rel, err)
			return nil
		}
		if string(got) != string(want) {
			t.Errorf("%s differs between the source and the copy", rel)
		}
		return nil
	})

	apt := newAptClient(t)
	apt.Trust(key.Public)
	apt.Source("file://"+dst, "bookworm", "main", "")
	apt.Update()
	apt.Download("hello")
}

// TestCopyTransformingRequiresItsOwnSignature covers the refusal and the
// successful re-signed transform.
func TestCopyTransformingRequiresItsOwnSignature(t *testing.T) {
	key := newSigningKey(t)
	src := t.TempDir()
	cli(t, "add", src,
		pkgPath(t, "hello_2.10-3_all.deb"),
		pkgPath(t, "libfoo_1.3.0-1_amd64.deb"),
		pkgPath(t, "libfoo_1.4.0-1_amd64.deb"),
		"--suite", "bookworm", "--sign-release", "--gpg-key-id", key.UID)
	keyring := key.KeyringFile(t)

	// Transforming a signed source without a key of our own must be refused.
	out, err := tryCLI(t, "copy", src, t.TempDir(), "--latest-only", "--keyring", keyring)
	if err == nil {
		t.Fatal("a transforming copy of a signed repository was allowed without a new signature")
	}
	if !strings.Contains(out, "--sign-release") {
		t.Errorf("the refusal does not say how to proceed:\n%s", out)
	}

	// With a key it succeeds, into a suite of its own.
	dst := t.TempDir()
	cli(t, "copy", src, dst, "--latest-only", "--to-suite", "trixie",
		"--keyring", keyring, "--sign-release", "--gpg-key-id", key.UID)

	apt := newAptClient(t)
	apt.Trust(key.Public)
	apt.Source("file://"+dst, "trixie", "main", "")
	apt.Update()
	if got := apt.Candidate("libfoo"); got != "1.4.0-1" {
		t.Errorf("the --latest-only copy offers libfoo %q; want 1.4.0-1", got)
	}
	if got := apt.Candidate("hello"); got != "2.10-3" {
		t.Errorf("the copy lost the arch:all package: candidate %q", got)
	}
}

// TestCopyFromReadOnlyHTTP mirrors a repository served over HTTP, which is the
// only backend that cannot enumerate its own contents.
func TestCopyFromReadOnlyHTTP(t *testing.T) {
	src := t.TempDir()
	cli(t, "add", src,
		pkgPath(t, "hello_2.10-3_all.deb"),
		pkgPath(t, "libfoo_1.3.0-1_amd64.deb"),
		"--suite", "bookworm")

	server := httptest.NewServer(http.FileServer(http.Dir(src)))
	defer server.Close()

	// check reads it over HTTP.
	cli(t, "check", "--level", "fetch", "--arch", "any", "--versions", "all",
		"--suite", "bookworm", server.URL)

	// copy mirrors it to local disk. The source cannot list itself, so the run
	// must say so rather than silently copying less than the whole thing.
	dst := t.TempDir()
	out := cli(t, "copy", server.URL, dst, "--suite", "bookworm")
	if !strings.Contains(out, "cannot list its contents") {
		t.Errorf("the copy did not warn that the HTTP source cannot be enumerated:\n%s", out)
	}

	apt := newAptClient(t)
	apt.Source("file://"+dst, "bookworm", "main", "trusted=yes")
	apt.Update()
	apt.Download("hello")
}

// TestTargetProfilesChangeTheIndexSet proves --target moves the compression and
// hash axes, and that the result is still readable.
func TestTargetProfilesChangeTheIndexSet(t *testing.T) {
	legacy := t.TempDir()
	cli(t, "add", legacy, pkgPath(t, "pinned_1.0-1_amd64.deb"), "--suite", "bookworm", "--target", "legacy")

	indexDir := filepath.Join(legacy, "dists", "bookworm", "main", "binary-amd64")
	if _, err := os.Stat(filepath.Join(indexDir, "Packages.xz")); !os.IsNotExist(err) {
		t.Errorf("the legacy profile wrote an xz index (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(indexDir, "Packages.gz")); err != nil {
		t.Errorf("the legacy profile wrote no gzip index: %v", err)
	}
	release, err := os.ReadFile(filepath.Join(legacy, "dists", "bookworm", "Release"))
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range []string{"MD5Sum:", "SHA1:", "SHA256:"} {
		if !strings.Contains(string(release), block) {
			t.Errorf("the legacy profile's Release has no %s block", block)
		}
	}

	modern := t.TempDir()
	cli(t, "add", modern, pkgPath(t, "pinned_1.0-1_amd64.deb"), "--suite", "bookworm", "--target", "jammy")
	for _, name := range []string{"Packages", "Packages.gz", "Packages.xz", "Packages.zst"} {
		p := filepath.Join(modern, "dists", "bookworm", "main", "binary-amd64", name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the zstd profile did not write %s: %v", name, err)
		}
	}

	apt := newAptClient(t)
	apt.Source("file://"+modern, "bookworm", "main", "trusted=yes")
	apt.Update()
	apt.Download("pinned")
}

// TestDryRunChangesNothing is the promise --dry-run makes.
func TestDryRunChangesNothing(t *testing.T) {
	dir := t.TempDir()
	out := cli(t, "add", dir, pkgPath(t, "hello_2.10-3_all.deb"), "--suite", "bookworm", "--dry-run")
	if !strings.Contains(out, "[dry-run]") {
		t.Errorf("the run did not mark its output as a dry run:\n%s", out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("--dry-run wrote %d entr(ies) into an empty directory", len(entries))
	}
}
