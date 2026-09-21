package aptdata

import (
	"sort"
	"strings"
)

// Entry is the common shape of a binary package and a source package: enough
// to identify it, order its versions, and decide where it belongs. Code that
// selects, prunes or reports on packages works through this so it covers both
// kinds without duplicating itself.
type Entry interface {
	// Name is the package name.
	Name() string
	// Version is the version string as published.
	Version() string
	// Arch is the architecture, or "source" for a source package.
	Arch() string
	// ParsedVersion returns the version in comparable form.
	ParsedVersion() Version
	// Key is the name+architecture slot within which versions supersede.
	Key() string
	// ID is the entry's identity within the repository: the sha256 of the .deb
	// or of the .dsc.
	ID() string
	// ID3 is the human-readable name_version[_arch] identifier.
	ID3() string
	// Locations lists every repo-root-relative file the entry owns. A binary
	// package owns one; a source package owns its .dsc and its tarballs.
	Locations() []string
	// TotalBytes is the combined size of those files.
	TotalBytes() int64
	// ComponentName is the section of the suite the entry is indexed in.
	ComponentName() string
}

// Locations returns the single file a binary package owns.
func (p *Package) Locations() []string { return []string{p.Location()} }

// TotalBytes returns the size of the .deb.
func (p *Package) TotalBytes() int64 { return p.Size() }

// ComponentName returns the package's component.
func (p *Package) ComponentName() string { return p.Component }

// Locations returns every file the source package owns, .dsc included.
func (s *Source) Locations() []string {
	dir := s.Directory()
	files := s.SourceFiles()
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Location(dir))
	}
	return out
}

// TotalBytes returns the combined size of the source's files.
func (s *Source) TotalBytes() int64 { return s.TotalSize() }

// ComponentName returns the source's component.
func (s *Source) ComponentName() string { return s.Component }

// Index is the in-memory view of one suite's package set, keyed by each
// entry's checksum. It is what add/remove mutate and what publish renders back
// into Packages and Sources indexes.
//
// An index covers exactly one suite. A repository with several suites has one
// index per suite; they share the pool but nothing else.
type Index struct {
	// Suite is the dists/ subdirectory this index describes.
	Suite string

	// Release holds the fields of the suite's loaded Release file other than
	// the checksum blocks, so a republish preserves Origin, Label, Codename and
	// anything else the repository was created with.
	Release *Paragraph

	byID    map[string]*Package
	srcByID map[string]*Source
}

// NewIndex returns an empty index for a suite.
func NewIndex(suite string) *Index {
	return &Index{
		Suite:   suite,
		Release: NewParagraph(),
		byID:    map[string]*Package{},
		srcByID: map[string]*Source{},
	}
}

// Add inserts or replaces a binary package by checksum.
func (idx *Index) Add(p *Package) { idx.byID[p.ID()] = p }

// AddSource inserts or replaces a source package by checksum.
func (idx *Index) AddSource(s *Source) { idx.srcByID[s.ID()] = s }

// AddEntry inserts either kind.
func (idx *Index) AddEntry(e Entry) {
	switch v := e.(type) {
	case *Package:
		idx.Add(v)
	case *Source:
		idx.AddSource(v)
	}
}

// Len returns the number of binary packages.
func (idx *Index) Len() int { return len(idx.byID) }

// SourceLen returns the number of source packages.
func (idx *Index) SourceLen() int { return len(idx.srcByID) }

// Total returns the number of entries of both kinds.
func (idx *Index) Total() int { return len(idx.byID) + len(idx.srcByID) }

// ByID returns the binary package with the given checksum, or nil.
func (idx *Index) ByID(id string) *Package { return idx.byID[id] }

// SourceByID returns the source package with the given checksum, or nil.
func (idx *Index) SourceByID(id string) *Source { return idx.srcByID[id] }

// EntryByID returns either kind of entry by checksum, or nil.
func (idx *Index) EntryByID(id string) Entry {
	if p, ok := idx.byID[id]; ok {
		return p
	}
	if s, ok := idx.srcByID[id]; ok {
		return s
	}
	return nil
}

// ByLocation returns the binary package published at the given path, or nil.
func (idx *Index) ByLocation(loc string) *Package {
	for _, p := range idx.byID {
		if p.Location() == loc {
			return p
		}
	}
	return nil
}

// EntryByLocation returns the entry that owns the given file, or nil. A source
// package owns several files, any of which identifies it.
func (idx *Index) EntryByLocation(loc string) Entry {
	if p := idx.ByLocation(loc); p != nil {
		return p
	}
	for _, s := range idx.srcByID {
		for _, l := range s.Locations() {
			if l == loc {
				return s
			}
		}
	}
	return nil
}

// RemoveByID deletes an entry of either kind by checksum, reporting whether it
// was present.
func (idx *Index) RemoveByID(id string) bool {
	if _, ok := idx.byID[id]; ok {
		delete(idx.byID, id)
		return true
	}
	if _, ok := idx.srcByID[id]; ok {
		delete(idx.srcByID, id)
		return true
	}
	return false
}

// Find returns every entry matching name, optionally constrained by arch and
// version (either empty means "any"). An arch of "source" matches only source
// packages; any other arch matches only binary packages.
func (idx *Index) Find(name, arch, version string) []Entry {
	var out []Entry
	if arch != ArchSource {
		for _, p := range idx.byID {
			if matches(p, name, arch, version) {
				out = append(out, p)
			}
		}
	}
	if arch == "" || arch == ArchSource {
		for _, s := range idx.srcByID {
			if matches(s, name, "", version) {
				out = append(out, s)
			}
		}
	}
	sortEntries(out)
	return out
}

func matches(e Entry, name, arch, version string) bool {
	if e.Name() != name {
		return false
	}
	if arch != "" && e.Arch() != arch {
		return false
	}
	if version != "" && e.Version() != version {
		return false
	}
	return true
}

// Remove deletes every entry matching the selector and returns them.
func (idx *Index) Remove(name, arch, version string) []Entry {
	removed := idx.Find(name, arch, version)
	for _, e := range removed {
		idx.RemoveByID(e.ID())
	}
	return removed
}

// Packages returns the binary packages in a deterministic order.
func (idx *Index) Packages() []*Package {
	out := make([]*Package, 0, len(idx.byID))
	for _, p := range idx.byID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return lessEntry(out[i], out[j]) })
	return out
}

// Sources returns the source packages in a deterministic order.
func (idx *Index) Sources() []*Source {
	out := make([]*Source, 0, len(idx.srcByID))
	for _, s := range idx.srcByID {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return lessEntry(out[i], out[j]) })
	return out
}

// Entries returns every entry of both kinds in a deterministic order, binary
// packages first.
func (idx *Index) Entries() []Entry {
	out := make([]Entry, 0, idx.Total())
	for _, p := range idx.Packages() {
		out = append(out, p)
	}
	for _, s := range idx.Sources() {
		out = append(out, s)
	}
	return out
}

// Components returns the components in use, sorted.
func (idx *Index) Components() []string {
	seen := map[string]bool{}
	for _, e := range idx.Entries() {
		if c := e.ComponentName(); c != "" {
			seen[c] = true
		}
	}
	return sortedKeys(seen)
}

// Architectures returns the binary architectures in use, sorted. The "all"
// pseudo-architecture is not returned on its own: an arch:all package is
// written into every real architecture's index, so it does not create one.
func (idx *Index) Architectures() []string {
	seen := map[string]bool{}
	for _, p := range idx.byID {
		if a := p.Arch(); a != "" && a != ArchAll {
			seen[a] = true
		}
	}
	return sortedKeys(seen)
}

// HasSources reports whether any source package is indexed.
func (idx *Index) HasSources() bool { return len(idx.srcByID) > 0 }

// Locations returns every repo-root-relative file the index references, mapped
// to the checksum of the entry that owns it.
func (idx *Index) Locations() map[string]string {
	out := map[string]string{}
	for _, e := range idx.Entries() {
		for _, loc := range e.Locations() {
			out[loc] = e.ID()
		}
	}
	return out
}

// sortEntries orders entries by name, architecture and version.
func sortEntries(es []Entry) {
	sort.Slice(es, func(i, j int) bool { return lessEntry(es[i], es[j]) })
}

// lessEntry is the canonical entry ordering: name, then architecture, then
// version (oldest first), then checksum so the order is total.
func lessEntry(a, b Entry) bool {
	if a.Name() != b.Name() {
		return a.Name() < b.Name()
	}
	if a.Arch() != b.Arch() {
		return a.Arch() < b.Arch()
	}
	if c := a.ParsedVersion().Compare(b.ParsedVersion()); c != 0 {
		return c < 0
	}
	return a.ID() < b.ID()
}

// SplitList splits a space- or comma-separated Release field such as
// Architectures or Components.
func SplitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ',' || r == '\n'
	})
}
