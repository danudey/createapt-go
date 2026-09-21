package aptdata

import (
	"fmt"
	"strings"
)

// ParsePackages reads a Packages index. Every stanza must carry the fields a
// repository needs to act on it — name, version, architecture, and the
// location/size/checksum triple that lets the file be found and validated —
// because an entry missing any of them describes a package apt cannot fetch.
//
// component is recorded on each package: it is not in the document, it comes
// from the index's position in the tree.
func ParsePackages(data []byte, component string) ([]*Package, error) {
	paras, err := ParseParagraphs(data)
	if err != nil {
		return nil, fmt.Errorf("parse Packages: %w", err)
	}
	out := make([]*Package, 0, len(paras))
	for i, para := range paras {
		p := &Package{Paragraph: para, Component: component}
		if err := validatePackage(p); err != nil {
			return nil, fmt.Errorf("parse Packages: stanza %d: %w", i+1, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func validatePackage(p *Package) error {
	for _, field := range []string{FieldPackage, FieldVersion, FieldArchitecture, FieldFilename, FieldSHA256} {
		if p.Get(field) == "" {
			return fmt.Errorf("missing %s field", field)
		}
	}
	if p.Size() <= 0 {
		return fmt.Errorf("package %s has a missing or zero Size", p.Name())
	}
	if _, err := ParseVersion(p.Version()); err != nil {
		return fmt.Errorf("package %s: %w", p.Name(), err)
	}
	return nil
}

// ParseSources reads a Sources index. As with ParsePackages, a stanza that does
// not name its files or say where they live is rejected.
func ParseSources(data []byte, component string) ([]*Source, error) {
	paras, err := ParseParagraphs(data)
	if err != nil {
		return nil, fmt.Errorf("parse Sources: %w", err)
	}
	out := make([]*Source, 0, len(paras))
	for i, para := range paras {
		s := &Source{Paragraph: para, Component: component}
		if err := validateSource(s); err != nil {
			return nil, fmt.Errorf("parse Sources: stanza %d: %w", i+1, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func validateSource(s *Source) error {
	for _, field := range []string{FieldPackage, FieldVersion, FieldDirectory} {
		if s.Get(field) == "" {
			return fmt.Errorf("missing %s field", field)
		}
	}
	files := s.SourceFiles()
	if len(files) == 0 {
		return fmt.Errorf("source %s lists no files", s.Name())
	}
	if s.ID() == "" {
		return fmt.Errorf("source %s has no .dsc with a recorded SHA256", s.Name())
	}
	if _, err := ParseVersion(s.Version()); err != nil {
		return fmt.Errorf("source %s: %w", s.Name(), err)
	}
	return nil
}

// WritePackages renders a Packages index. Packages are emitted in the index's
// canonical order so a rebuild of an unchanged repository produces a
// byte-identical document.
func WritePackages(pkgs []*Package) []byte {
	paras := make([]*Paragraph, 0, len(pkgs))
	for _, p := range pkgs {
		paras = append(paras, p.Paragraph)
	}
	return WriteParagraphs(paras)
}

// WriteSources renders a Sources index.
func WriteSources(srcs []*Source) []byte {
	paras := make([]*Paragraph, 0, len(srcs))
	for _, s := range srcs {
		paras = append(paras, s.Paragraph)
	}
	return WriteParagraphs(paras)
}

// packagesFieldOrder is the field order a freshly derived binary stanza is
// emitted in: dpkg's control order, then the fields a repository adds. It
// applies only to stanzas this tool builds from a .deb; one read back from a
// published index keeps the order it was published with.
var packagesFieldOrder = []string{
	FieldPackage, FieldSource, FieldVersion, FieldArchitecture,
	"Essential", "Installed-Size", "Maintainer", "Original-Maintainer",
	"Multi-Arch", "Homepage", FieldSection, FieldPriority,
	FieldPreDepends, FieldDepends, FieldRecommends, FieldSuggests,
	FieldEnhances, FieldBreaks, FieldConflicts, FieldProvides, FieldReplaces,
	"Built-Using", "Static-Built-Using",
	FieldFilename, FieldSize, FieldMD5sum, FieldSHA1, FieldSHA256,
	FieldDescription, FieldDescriptionM,
}

// sourcesFieldOrder is the equivalent for a freshly derived source stanza.
var sourcesFieldOrder = []string{
	FieldPackage, "Binary", FieldReleaseVer, "Maintainer", "Uploaders",
	FieldArchitecture, "Standards-Version", "Format", "Homepage", "Vcs-Browser",
	"Vcs-Git", FieldSection, FieldPriority,
	"Build-Depends", "Build-Depends-Indep", "Build-Depends-Arch",
	"Build-Conflicts", "Build-Conflicts-Indep", "Build-Conflicts-Arch",
	"Package-List", FieldDirectory,
	FieldFiles, FieldChecksumsSHA1, FieldChecksumsSHA256,
}

// Reorder rewrites a paragraph's fields into the given order, keeping any
// field the order does not mention in its original relative position at the
// end. It is applied to stanzas this tool derives from a package file so they
// read the way a Debian index does.
func Reorder(p *Paragraph, order []string) *Paragraph {
	out := NewParagraph()
	emitted := map[string]bool{}
	for _, name := range order {
		if p.Has(name) {
			out.Set(name, p.Get(name))
			emitted[strings.ToLower(name)] = true
		}
	}
	for _, f := range p.Fields {
		if !emitted[strings.ToLower(f.Name)] {
			out.Set(f.Name, f.Value)
		}
	}
	return out
}

// CanonicalizePackage reorders a derived binary stanza into index order.
func CanonicalizePackage(p *Package) { p.Paragraph = Reorder(p.Paragraph, packagesFieldOrder) }

// CanonicalizeSource reorders a derived source stanza into index order.
func CanonicalizeSource(s *Source) { s.Paragraph = Reorder(s.Paragraph, sourcesFieldOrder) }

// SetChecksums records a binary package's file size and digests in the fields
// a Packages index carries them in, dropping any algorithm not supplied.
func SetChecksums(p *Package, size int64, h Hashes) {
	p.Set(FieldSize, fmt.Sprintf("%d", size))
	for algo, field := range packagesField {
		if v, ok := h[algo]; ok {
			p.Set(field, v)
		} else {
			p.Delete(field)
		}
	}
}
