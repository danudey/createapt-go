package aptdata

import (
	"strings"
	"testing"
	"time"
)

const sampleRelease = `Origin: Example
Label: Example
Suite: bookworm
Codename: bookworm
Date: Sat, 09 Dec 2023 10:00:00 UTC
Acquire-By-Hash: yes
Architectures: amd64 arm64
Components: main contrib
MD5Sum:
 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa             1143 main/binary-amd64/Packages
 bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb              641 main/binary-amd64/Packages.gz
SHA256:
 cccc                                         1143 main/binary-amd64/Packages
 dddd                                          641 main/binary-amd64/Packages.gz
`

func TestParseRelease(t *testing.T) {
	rel, err := ParseRelease([]byte(sampleRelease))
	if err != nil {
		t.Fatal(err)
	}

	if got := rel.Suite(); got != "bookworm" {
		t.Errorf("Suite() = %q; want bookworm", got)
	}
	if got := strings.Join(rel.Architectures(), ","); got != "amd64,arm64" {
		t.Errorf("Architectures() = %q; want amd64,arm64", got)
	}
	if got := strings.Join(rel.Components(), ","); got != "main,contrib" {
		t.Errorf("Components() = %q; want main,contrib", got)
	}
	if !rel.AcquireByHash() {
		t.Error("AcquireByHash() = false; want true")
	}
	if len(rel.Files) != 2 {
		t.Fatalf("got %d files; want 2", len(rel.Files))
	}

	// The two checksum blocks describe the same files, so they must merge onto
	// one entry per path rather than producing one entry per block.
	f := rel.FileByPath("main/binary-amd64/Packages")
	if f == nil {
		t.Fatal("no entry for main/binary-amd64/Packages")
	}
	if f.Size != 1143 {
		t.Errorf("size = %d; want 1143", f.Size)
	}
	if f.Hashes[HashMD5] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || f.Hashes[HashSHA256] != "cccc" {
		t.Errorf("hashes = %v; want the md5 and sha256 from both blocks", f.Hashes)
	}

	// The descriptive fields must not have absorbed the checksum blocks.
	if rel.Para.Has("SHA256") || rel.Para.Has("MD5Sum") {
		t.Error("a checksum block was left among the descriptive fields")
	}
}

func TestReleaseRoundTrip(t *testing.T) {
	rel, err := ParseRelease([]byte(sampleRelease))
	if err != nil {
		t.Fatal(err)
	}
	rendered := rel.Render()

	again, err := ParseRelease(rendered)
	if err != nil {
		t.Fatalf("re-parsing a rendered Release failed: %v", err)
	}
	if again.Suite() != rel.Suite() || len(again.Files) != len(rel.Files) {
		t.Errorf("round trip changed the Release: suite %q->%q, %d->%d files",
			rel.Suite(), again.Suite(), len(rel.Files), len(again.Files))
	}
	for _, f := range rel.Files {
		g := again.FileByPath(f.Path)
		if g == nil {
			t.Errorf("%s vanished in the round trip", f.Path)
			continue
		}
		if g.Size != f.Size {
			t.Errorf("%s size %d -> %d", f.Path, f.Size, g.Size)
		}
		for algo, want := range f.Hashes {
			if g.Hashes[algo] != want {
				t.Errorf("%s %s %q -> %q", f.Path, algo, want, g.Hashes[algo])
			}
		}
	}
}

func TestReleasePreservesUnknownFields(t *testing.T) {
	doc := sampleRelease + "X-Vendor-Note: keep me\n"
	rel, err := ParseRelease([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rel.Render()), "X-Vendor-Note: keep me") {
		t.Error("a field the tool does not manage was dropped on render")
	}
}

func TestReleaseDateAndValidity(t *testing.T) {
	rel := NewRelease()
	when := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	rel.SetDate(when, 0)
	if got := rel.Date(); !got.Equal(when) {
		t.Errorf("Date() = %v; want %v", got, when)
	}
	if !rel.ValidUntil().IsZero() {
		t.Error("a zero validity still wrote a Valid-Until")
	}

	rel.SetDate(when, 48*time.Hour)
	want := when.Add(48 * time.Hour)
	if got := rel.ValidUntil(); !got.Equal(want) {
		t.Errorf("ValidUntil() = %v; want %v", got, want)
	}

	// Setting a zero validity again must clear the field, not leave the old
	// expiry behind on a repository that is no longer meant to expire.
	rel.SetDate(when, 0)
	if !rel.ValidUntil().IsZero() {
		t.Error("Valid-Until survived being set back to no expiry")
	}
}

func TestParseHashesAlwaysIncludesSHA256(t *testing.T) {
	got, err := ParseHashes("md5")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "md5,sha256" {
		t.Errorf("ParseHashes(%q) = %v; want md5 and sha256 in weakest-first order", "md5", got)
	}
	if _, err := ParseHashes("crc32"); err == nil {
		t.Error("an unknown hash was accepted")
	}
}

func TestParseCompressionsAlwaysIncludesPlain(t *testing.T) {
	got, err := ParseCompressions("xz,gzip")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != None || got[1] != GZIP || got[2] != XZ {
		t.Errorf("ParseCompressions(%q) = %v; want none, gzip, xz", "xz,gzip", got)
	}
	if _, err := ParseCompressions("lzo"); err == nil {
		t.Error("an unknown compression was accepted")
	}
}

func TestCompressRoundTrip(t *testing.T) {
	body := []byte(strings.Repeat("Package: hello\nVersion: 1.0\n\n", 200))
	for _, c := range []Compression{None, GZIP, XZ, ZSTD} {
		packed, err := c.Compress(body)
		if err != nil {
			t.Fatalf("%s: compress: %v", c, err)
		}
		got, err := Decompress("Packages"+c.Ext(), packed)
		if err != nil {
			t.Fatalf("%s: decompress: %v", c, err)
		}
		if string(got) != string(body) {
			t.Errorf("%s: round trip changed the document", c)
		}
	}
}

func TestByHashPath(t *testing.T) {
	got := ByHashPath("dists/bookworm/main/binary-amd64", HashSHA256, "deadbeef")
	want := "dists/bookworm/main/binary-amd64/by-hash/SHA256/deadbeef"
	if got != want {
		t.Errorf("ByHashPath = %q; want %q", got, want)
	}
}

func TestPoolPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"hello":  "h",
		"libfoo": "libf",
		"lib":    "l",
		"zsh":    "z",
	} {
		if got := PoolPrefix(in); got != want {
			t.Errorf("PoolPrefix(%q) = %q; want %q", in, got, want)
		}
	}
	if got := PoolPath("main", "libfoo", "libfoo_1.0_amd64.deb"); got != "pool/main/libf/libfoo/libfoo_1.0_amd64.deb" {
		t.Errorf("PoolPath = %q", got)
	}
}

func TestValidSuiteRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"", "..", "a/../b", "/abs", "rel/", "sp ace"} {
		if err := ValidSuite(bad); err == nil {
			t.Errorf("ValidSuite(%q) accepted it", bad)
		}
	}
	for _, good := range []string{"stable", "bookworm", "jammy-updates", "1.0"} {
		if err := ValidSuite(good); err != nil {
			t.Errorf("ValidSuite(%q) rejected it: %v", good, err)
		}
	}
	// A component may span directories, as Debian's "updates/main" does.
	if err := ValidComponent("updates/main"); err != nil {
		t.Errorf("ValidComponent(updates/main) rejected it: %v", err)
	}
	if err := ValidArch("amd64/x"); err == nil {
		t.Error("ValidArch accepted an architecture containing a slash")
	}
}
