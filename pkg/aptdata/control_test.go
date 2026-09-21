package aptdata

import (
	"strings"
	"testing"
)

const sampleStanza = `Package: hello
Version: 2.10-3
Architecture: all
Depends: libfoo (>= 1.3.0), libbar | libbaz
Description: friendly greeting program
 A minimal package.
 .
 It has no other purpose.
`

func TestParseParagraphsReadsFoldedFields(t *testing.T) {
	paras, err := ParseParagraphs([]byte(sampleStanza))
	if err != nil {
		t.Fatal(err)
	}
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs; want 1", len(paras))
	}
	p := paras[0]

	if got := p.Get("Package"); got != "hello" {
		t.Errorf("Package = %q; want hello", got)
	}
	// Field names are case-insensitive.
	if got := p.Get("pAcKaGe"); got != "hello" {
		t.Errorf("case-insensitive lookup returned %q; want hello", got)
	}
	// A " ." continuation line is a blank line inside the value.
	want := "friendly greeting program\nA minimal package.\n\nIt has no other purpose."
	if got := p.Get("Description"); got != want {
		t.Errorf("Description = %q; want %q", got, want)
	}
}

func TestParagraphRoundTrip(t *testing.T) {
	paras, err := ParseParagraphs([]byte(sampleStanza))
	if err != nil {
		t.Fatal(err)
	}
	got := string(paras[0].Render())
	if got != sampleStanza {
		t.Errorf("round trip changed the stanza:\n--- got ---\n%s\n--- want ---\n%s", got, sampleStanza)
	}
}

func TestParseParagraphsSeparatesStanzas(t *testing.T) {
	doc := "Package: a\nVersion: 1\n\nPackage: b\nVersion: 2\n"
	paras, err := ParseParagraphs([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(paras) != 2 {
		t.Fatalf("got %d paragraphs; want 2", len(paras))
	}
	if paras[0].Get("Package") != "a" || paras[1].Get("Package") != "b" {
		t.Errorf("stanzas parsed as %q and %q; want a and b",
			paras[0].Get("Package"), paras[1].Get("Package"))
	}
	// WriteParagraphs must produce a document that parses back to the same set.
	again, err := ParseParagraphs(WriteParagraphs(paras))
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 {
		t.Errorf("re-parsed %d paragraphs; want 2", len(again))
	}
}

func TestParagraphSetPreservesPositionAndDeleteRemoves(t *testing.T) {
	p := NewParagraph()
	p.Set("Package", "hello")
	p.Set("Version", "1.0")
	p.Set("Architecture", "all")

	p.Set("Version", "2.0")
	if got := string(p.Render()); !strings.Contains(got, "Package: hello\nVersion: 2.0\nArchitecture: all\n") {
		t.Errorf("Set on an existing field moved it:\n%s", got)
	}

	if !p.Delete("Version") {
		t.Error("Delete reported the field was absent")
	}
	if p.Has("Version") {
		t.Error("the field is still present after Delete")
	}
	// The remaining fields must still be addressable, which fails if Delete
	// leaves the position index stale.
	if got := p.Get("Architecture"); got != "all" {
		t.Errorf("Architecture = %q after deleting an earlier field; want all", got)
	}
}

func TestParseParagraphsRejectsMalformed(t *testing.T) {
	for name, doc := range map[string]string{
		"continuation with no field": " orphaned continuation\n",
		"field with no colon":        "Package hello\n",
		"empty field name":           ": value\n",
	} {
		if _, err := ParseParagraphs([]byte(doc)); err == nil {
			t.Errorf("%s: parsed without error; want a failure", name)
		}
	}
}

func TestSourceFilesMergeChecksumBlocks(t *testing.T) {
	doc := `Package: foo
Version: 1.0-1
Directory: pool/main/f/foo
Files:
 d41d8cd98f00b204e9800998ecf8427e 10 foo_1.0.orig.tar.gz
 0800fc577294c34e0b28ad2839435945 20 foo_1.0-1.dsc
Checksums-Sha256:
 aaaa 10 foo_1.0.orig.tar.gz
 bbbb 20 foo_1.0-1.dsc
`
	srcs, err := ParseSources([]byte(doc), "main")
	if err != nil {
		t.Fatal(err)
	}
	files := srcs[0].SourceFiles()
	if len(files) != 2 {
		t.Fatalf("got %d files; want 2", len(files))
	}
	// Ordered by name, so the .dsc comes first.
	if files[0].Name != "foo_1.0-1.dsc" || files[0].SHA256 != "bbbb" || files[0].Size != 20 {
		t.Errorf("first file = %+v; want the .dsc with sha256 bbbb and size 20", files[0])
	}
	if files[0].MD5 != "0800fc577294c34e0b28ad2839435945" {
		t.Errorf("the md5 from Files was not merged onto the sha256 entry: %+v", files[0])
	}
	if got := srcs[0].ID(); got != "bbbb" {
		t.Errorf("ID() = %q; want the .dsc's sha256 bbbb", got)
	}
}

func TestSetSourceFilesRoundTrips(t *testing.T) {
	s := NewSource(NewParagraph())
	s.Set(FieldPackage, "foo")
	s.Set(FieldVersion, "1.0-1")
	s.SetDirectory("pool/main/f/foo")
	s.SetSourceFiles([]SourceFile{
		{Name: "foo_1.0-1.dsc", Size: 20, MD5: "m2", SHA256: "bbbb"},
		{Name: "foo_1.0.orig.tar.gz", Size: 10, MD5: "m1", SHA256: "aaaa"},
	})

	// No SHA1 was supplied, so the field must not appear at all.
	if s.Has(FieldChecksumsSHA1) {
		t.Error("Checksums-Sha1 was written although no sha1 was supplied")
	}

	files := s.SourceFiles()
	if len(files) != 2 {
		t.Fatalf("got %d files back; want 2", len(files))
	}
	if files[1].Name != "foo_1.0.orig.tar.gz" || files[1].SHA256 != "aaaa" {
		t.Errorf("second file = %+v; want the tarball with sha256 aaaa", files[1])
	}
	if got := s.TotalSize(); got != 30 {
		t.Errorf("TotalSize() = %d; want 30", got)
	}
}
