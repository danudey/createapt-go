package debmeta

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createapt-go/pkg/aptdata"
)

// referenceDir locates the sample packages built by reference/gen.sh.
func referenceDir(t *testing.T) string {
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

func TestPackageFromFile(t *testing.T) {
	dir := referenceDir(t)
	path := filepath.Join(dir, "hello_2.10-3_all.deb")

	pkg, err := PackageFromFile(path, Options{Component: "main", PoolLayout: true})
	if err != nil {
		t.Fatal(err)
	}

	if pkg.Name() != "hello" || pkg.Version() != "2.10-3" || pkg.Arch() != "all" {
		t.Errorf("identity = %s; want hello_2.10-3_all", pkg.ID3())
	}
	if got := pkg.Location(); got != "pool/main/h/hello/hello_2.10-3_all.deb" {
		t.Errorf("Location() = %q; want the conventional pool path", got)
	}

	// The size and checksums must describe the file on disk, since that is
	// what apt checks the download against.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Size() != fi.Size() {
		t.Errorf("Size() = %d; want %d", pkg.Size(), fi.Size())
	}
	if len(pkg.ID()) != 64 {
		t.Errorf("ID() = %q; want a hex sha256", pkg.ID())
	}

	// Control fields must survive into the index stanza.
	if got := pkg.Get(aptdata.FieldDepends); got != "libfoo (>= 1.3.0)" {
		t.Errorf("Depends = %q; want it carried over from the control file", got)
	}
	if !strings.HasPrefix(pkg.Get(aptdata.FieldDescription), "friendly greeting program\n") {
		t.Errorf("Description did not survive as a folded field: %q", pkg.Get(aptdata.FieldDescription))
	}
}

func TestPackageFromFileUsesLibPoolPrefixAndSourceName(t *testing.T) {
	dir := referenceDir(t)
	pkg, err := PackageFromFile(filepath.Join(dir, "libfoo_1.3.0-1_amd64.deb"),
		Options{Component: "main", PoolLayout: true})
	if err != nil {
		t.Fatal(err)
	}
	// libfoo declares "Source: foo", so it lives under the source's pool
	// directory, not its own.
	if got := pkg.Location(); got != "pool/main/f/foo/libfoo_1.3.0-1_amd64.deb" {
		t.Errorf("Location() = %q; want it under the source package's pool directory", got)
	}
	if got := pkg.SourceName(); got != "foo" {
		t.Errorf("SourceName() = %q; want foo", got)
	}
}

func TestPackageFromFileFlatLayout(t *testing.T) {
	dir := referenceDir(t)
	pkg, err := PackageFromFile(filepath.Join(dir, "hello_2.10-3_all.deb"),
		Options{Component: "contrib", PoolLayout: false})
	if err != nil {
		t.Fatal(err)
	}
	if got := pkg.Location(); got != "pool/contrib/hello_2.10-3_all.deb" {
		t.Errorf("Location() = %q; want a flat pool path in the named component", got)
	}
}

// TestControlMatchesDpkgDeb compares the control stanza this package extracts
// against the one dpkg-deb itself reports, field by field. It is the check that
// the pure-Go ar and tar walk agrees with the tool apt's own ecosystem uses.
func TestControlMatchesDpkgDeb(t *testing.T) {
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb is not installed")
	}
	dir := referenceDir(t)

	for _, name := range []string{"hello_2.10-3_all.deb", "libfoo_1.3.0-1_amd64.deb"} {
		path := filepath.Join(dir, name)

		ours, err := ControlFromDeb(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out, err := exec.Command("dpkg-deb", "--field", path).Output()
		if err != nil {
			t.Fatalf("%s: dpkg-deb --field: %v", name, err)
		}
		theirs, err := aptdata.ParseParagraphs(out)
		if err != nil {
			t.Fatalf("%s: parsing dpkg-deb output: %v", name, err)
		}
		if len(theirs) != 1 {
			t.Fatalf("%s: dpkg-deb produced %d stanzas", name, len(theirs))
		}

		for _, f := range theirs[0].Fields {
			if got := ours.Get(f.Name); got != f.Value {
				t.Errorf("%s: field %s = %q; dpkg-deb says %q", name, f.Name, got, f.Value)
			}
		}
		if len(ours.Fields) != len(theirs[0].Fields) {
			t.Errorf("%s: extracted %d fields, dpkg-deb reports %d", name, len(ours.Fields), len(theirs[0].Fields))
		}
	}
}

func TestSourceFromFile(t *testing.T) {
	dir := referenceDir(t)
	sf, err := SourceFromFile(filepath.Join(dir, "foo_1.3.0-1.dsc"),
		Options{Component: "main", PoolLayout: true})
	if err != nil {
		t.Fatal(err)
	}

	src := sf.Source
	if src.Name() != "foo" || src.Version() != "1.3.0-1" {
		t.Errorf("identity = %s; want foo_1.3.0-1", src.ID3())
	}
	// In a Sources index the name lives in Package, not Source.
	if src.Has(aptdata.FieldSource) {
		t.Error("the Source field survived into the index stanza; it should have become Package")
	}
	if got := src.Directory(); got != "pool/main/f/foo" {
		t.Errorf("Directory() = %q; want the conventional pool directory", got)
	}

	// The .dsc plus the two tarballs it names.
	files := src.SourceFiles()
	if len(files) != 3 {
		t.Fatalf("got %d files; want 3: %+v", len(files), files)
	}
	var sawDSC bool
	for _, f := range files {
		if strings.HasSuffix(f.Name, ".dsc") {
			sawDSC = true
		}
		if f.SHA256 == "" || f.Size == 0 {
			t.Errorf("file %s has no checksum or size: %+v", f.Name, f)
		}
	}
	if !sawDSC {
		t.Error("the .dsc itself is not listed among the source's files")
	}
	if src.ID() == "" {
		t.Error("ID() is empty; it should be the .dsc's sha256")
	}

	// Every file must be staged for upload at its pool destination.
	if len(sf.Files) != 3 {
		t.Errorf("staged %d files for upload; want 3: %v", len(sf.Files), sf.Files)
	}
	for dest := range sf.Files {
		if !strings.HasPrefix(dest, "pool/main/f/foo/") {
			t.Errorf("staged destination %q is outside the source's pool directory", dest)
		}
	}
}

// TestSourceFromFileRejectsAlteredTarball proves the .dsc's own checksums are
// enforced: a repository must never index a source package whose tarball does
// not match what the .dsc says it is.
func TestSourceFromFileRejectsAlteredTarball(t *testing.T) {
	dir := referenceDir(t)
	tmp := t.TempDir()

	for _, name := range []string{"foo_1.3.0-1.dsc", "foo_1.3.0.orig.tar.gz", "foo_1.3.0-1.debian.tar.gz"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "foo_1.3.0.orig.tar.gz" {
			data = append(data, "tampered"...)
		}
		if err := os.WriteFile(filepath.Join(tmp, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := SourceFromFile(filepath.Join(tmp, "foo_1.3.0-1.dsc"), Options{Component: "main"})
	if err == nil {
		t.Fatal("a tarball that does not match the .dsc was accepted")
	}
	if !strings.Contains(err.Error(), "foo_1.3.0.orig.tar.gz") {
		t.Errorf("the error does not name the offending file: %v", err)
	}
}

func TestStripClearsign(t *testing.T) {
	signed := `-----BEGIN PGP SIGNED MESSAGE-----
Hash: SHA512

Origin: Example
Suite: bookworm
- -----not a real armor line-----
-----BEGIN PGP SIGNATURE-----

aGVsbG8=
-----END PGP SIGNATURE-----
`
	want := "Origin: Example\nSuite: bookworm\n-----not a real armor line-----\n"
	if got := string(StripClearsign([]byte(signed))); got != want {
		t.Errorf("StripClearsign =\n%q\nwant\n%q", got, want)
	}

	// An unsigned document passes through untouched.
	plain := []byte("Origin: Example\n")
	if got := StripClearsign(plain); string(got) != string(plain) {
		t.Errorf("an unsigned document was altered: %q", got)
	}
}

func TestIsDebAndIsDSC(t *testing.T) {
	for path, wantDeb := range map[string]bool{
		"a.deb": true, "a.udeb": true, "A.DEB": true,
		"a.dsc": false, "a.tar.gz": false, "a": false,
	} {
		if got := IsDeb(path); got != wantDeb {
			t.Errorf("IsDeb(%q) = %v; want %v", path, got, wantDeb)
		}
	}
	if !IsDSC("foo_1.0-1.dsc") || IsDSC("foo.deb") {
		t.Error("IsDSC misclassified a path")
	}
}
