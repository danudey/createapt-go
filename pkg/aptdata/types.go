package aptdata

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Field names this package reads or writes. They are spelled here once so the
// conventional capitalization is used consistently on output; lookups are
// case-insensitive regardless.
const (
	FieldPackage         = "Package"
	FieldSource          = "Source"
	FieldVersion         = "Version"
	FieldArchitecture    = "Architecture"
	FieldFilename        = "Filename"
	FieldDirectory       = "Directory"
	FieldSize            = "Size"
	FieldMD5sum          = "MD5sum"
	FieldSHA1            = "SHA1"
	FieldSHA256          = "SHA256"
	FieldDescription     = "Description"
	FieldDescriptionM    = "Description-md5"
	FieldSection         = "Section"
	FieldPriority        = "Priority"
	FieldFiles           = "Files"
	FieldChecksumsSHA1   = "Checksums-Sha1"
	FieldChecksumsSHA256 = "Checksums-Sha256"
)

// ArchAll is the architecture of a package that installs on every architecture.
// Such a package is listed in every binary-<arch> index, because apt only reads
// the index for the architectures it is configured for.
const ArchAll = "all"

// ArchSource is the pseudo-architecture of a source package.
const ArchSource = "source"

// Package is one binary package's stanza in a Packages index.
//
// The whole stanza is kept rather than a fixed set of typed fields, so fields
// this tool does not interpret (Multi-Arch, Built-Using, Python-Version, any
// vendor X- field) survive a rebuild untouched. Typed accessors cover the
// fields the repository logic needs.
type Package struct {
	*Paragraph

	// Component is the section of the suite the package is indexed in
	// ("main", "contrib", ...). It is repository placement, not a control
	// field, so it is tracked here rather than in the stanza: it comes from
	// the index directory the package was read from and decides which index it
	// is written back to.
	Component string
}

// NewPackage wraps a stanza as a binary package.
func NewPackage(p *Paragraph) *Package { return &Package{Paragraph: p} }

// Name is the binary package name.
func (p *Package) Name() string { return p.Get(FieldPackage) }

// Version is the version string exactly as published.
func (p *Package) Version() string { return p.Get(FieldVersion) }

// Arch is the package's architecture ("amd64", "all", ...).
func (p *Package) Arch() string { return p.Get(FieldArchitecture) }

// Location is the package's repo-root-relative path: the Filename field, which
// is what a client resolves against the repository's base URL to fetch it.
func (p *Package) Location() string { return p.Get(FieldFilename) }

// SetLocation records a new Filename.
func (p *Package) SetLocation(loc string) { p.Set(FieldFilename, loc) }

// Size is the size of the .deb in bytes.
func (p *Package) Size() int64 { return parseInt64(p.Get(FieldSize)) }

// ID is the package's identity within a repository: the sha256 of the .deb.
// It is what lets an already-uploaded file be recognized as the same package
// without downloading it, and what the index is keyed by.
func (p *Package) ID() string { return strings.ToLower(p.Get(FieldSHA256)) }

// SourceName is the source package this binary was built from, defaulting to
// the binary's own name when the Source field is absent (as dpkg does).
func (p *Package) SourceName() string {
	src := p.Get(FieldSource)
	if src == "" {
		return p.Name()
	}
	// "Source: foo (1.2-3)" names a source version differing from the binary's.
	if i := strings.Index(src, "("); i > 0 {
		return strings.TrimSpace(src[:i])
	}
	return strings.TrimSpace(src)
}

// SourceVersion is the version of the source package this binary was built
// from, which differs from the binary version only for a binNMU.
func (p *Package) SourceVersion() string {
	src := p.Get(FieldSource)
	if i := strings.Index(src, "("); i > 0 {
		if j := strings.Index(src[i:], ")"); j > 0 {
			return strings.TrimSpace(src[i+1 : i+j])
		}
	}
	return p.Version()
}

// ParsedVersion returns the version in comparable form.
func (p *Package) ParsedVersion() Version { return MustParseVersion(p.Version()) }

// Key identifies a package's name+architecture slot: the granularity at which
// versions supersede one another.
func (p *Package) Key() string { return p.Name() + ":" + p.Arch() }

// ID3 renders the canonical name_version_arch identifier used in diagnostics
// and in the conventional .deb filename.
func (p *Package) ID3() string {
	return fmt.Sprintf("%s_%s_%s", p.Name(), p.Version(), p.Arch())
}

// Checksums returns the hashes recorded for the .deb, keyed by algorithm.
func (p *Package) Checksums() map[string]string {
	out := map[string]string{}
	for field, algo := range map[string]string{
		FieldMD5sum: HashMD5, FieldSHA1: HashSHA1, FieldSHA256: HashSHA256,
	} {
		if v := p.Get(field); v != "" {
			out[algo] = strings.ToLower(v)
		}
	}
	return out
}

// Source is one source package's stanza in a Sources index. Unlike a binary
// package it names several files (the .dsc plus its tarballs), all relative to
// its Directory.
type Source struct {
	*Paragraph

	// Component is the section of the suite the source is indexed in. See
	// Package.Component.
	Component string
}

// NewSource wraps a stanza as a source package.
func NewSource(p *Paragraph) *Source { return &Source{Paragraph: p} }

// Name is the source package name.
func (s *Source) Name() string { return s.Get(FieldPackage) }

// Version is the source version.
func (s *Source) Version() string { return s.Get(FieldVersion) }

// Arch reports the pseudo-architecture "source", so a Source can be handled
// alongside binary packages where only the architecture matters.
func (s *Source) Arch() string { return ArchSource }

// Directory is the repo-root-relative pool directory holding the source's
// files.
func (s *Source) Directory() string { return strings.Trim(s.Get(FieldDirectory), "/") }

// SetDirectory records a new pool directory.
func (s *Source) SetDirectory(dir string) { s.Set(FieldDirectory, strings.Trim(dir, "/")) }

// ParsedVersion returns the version in comparable form.
func (s *Source) ParsedVersion() Version { return MustParseVersion(s.Version()) }

// Key identifies the source's name+architecture slot.
func (s *Source) Key() string { return s.Name() + ":" + ArchSource }

// ID3 renders the canonical name_version identifier used in diagnostics.
func (s *Source) ID3() string { return fmt.Sprintf("%s_%s", s.Name(), s.Version()) }

// ID is the source package's identity within a repository: the sha256 of its
// .dsc file, which transitively covers every other file it names.
func (s *Source) ID() string {
	for _, f := range s.SourceFiles() {
		if strings.HasSuffix(f.Name, ".dsc") {
			return strings.ToLower(f.SHA256)
		}
	}
	return ""
}

// SourceFile is one file belonging to a source package.
type SourceFile struct {
	Name   string // basename, relative to the source's Directory
	Size   int64
	MD5    string
	SHA1   string
	SHA256 string
}

// Location returns the file's repo-root-relative path.
func (f SourceFile) Location(dir string) string { return path.Join(dir, f.Name) }

// SourceFiles merges the Files (md5), Checksums-Sha1 and Checksums-Sha256
// fields into one record per file, ordered by name. Each of those fields is a
// folded list of "<hash> <size> <name>" lines covering the same set of files.
func (s *Source) SourceFiles() []SourceFile {
	byName := map[string]*SourceFile{}
	for _, spec := range []struct {
		field string
		algo  string
	}{
		{FieldFiles, HashMD5},
		{FieldChecksumsSHA1, HashSHA1},
		{FieldChecksumsSHA256, HashSHA256},
	} {
		for _, line := range strings.Split(s.Get(spec.field), "\n") {
			hash, size, name, ok := parseChecksumLine(line)
			if !ok {
				continue
			}
			f := byName[name]
			if f == nil {
				f = &SourceFile{Name: name, Size: size}
				byName[name] = f
			}
			switch spec.algo {
			case HashMD5:
				f.MD5 = hash
			case HashSHA1:
				f.SHA1 = hash
			case HashSHA256:
				f.SHA256 = hash
			}
		}
	}
	out := make([]SourceFile, 0, len(byName))
	for _, name := range sortedKeys(byName) {
		out = append(out, *byName[name])
	}
	return out
}

// SetSourceFiles rewrites the Files/Checksums-* fields from the given records.
// A hash absent from every record leaves its field out, so a source indexed
// without SHA1 does not gain an empty one.
func (s *Source) SetSourceFiles(files []SourceFile) {
	sorted := append([]SourceFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	for _, spec := range []struct {
		field string
		get   func(SourceFile) string
	}{
		{FieldFiles, func(f SourceFile) string { return f.MD5 }},
		{FieldChecksumsSHA1, func(f SourceFile) string { return f.SHA1 }},
		{FieldChecksumsSHA256, func(f SourceFile) string { return f.SHA256 }},
	} {
		var b strings.Builder
		for _, f := range sorted {
			h := spec.get(f)
			if h == "" {
				continue
			}
			fmt.Fprintf(&b, "\n%s %d %s", h, f.Size, f.Name)
		}
		if b.Len() == 0 {
			s.Delete(spec.field)
			continue
		}
		// The value opens empty so every entry renders as a continuation line,
		// which is the folded form apt expects for these fields.
		s.Set(spec.field, b.String())
	}
}

// TotalSize sums the sizes of every file the source names.
func (s *Source) TotalSize() int64 {
	var n int64
	for _, f := range s.SourceFiles() {
		n += f.Size
	}
	return n
}

// parseChecksumLine splits "<hash> <size> <name>" as it appears in a Files,
// Checksums-* or Release checksum block.
func parseChecksumLine(line string) (hash string, size int64, name string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return "", 0, "", false
	}
	n, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return "", 0, "", false
	}
	return strings.ToLower(fields[0]), n, fields[2], true
}

func parseInt64(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
