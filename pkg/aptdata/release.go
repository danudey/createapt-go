package aptdata

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Release field names.
const (
	FieldOrigin        = "Origin"
	FieldLabel         = "Label"
	FieldSuite         = "Suite"
	FieldCodename      = "Codename"
	FieldReleaseVer    = "Version"
	FieldDate          = "Date"
	FieldValidUntil    = "Valid-Until"
	FieldAcquireByHash = "Acquire-By-Hash"
	FieldArchitectures = "Architectures"
	FieldComponents    = "Components"
	FieldReleaseDesc   = "Description"
	FieldNotAutomatic  = "NotAutomatic"
	FieldButAutoUpg    = "ButAutomaticUpgrades"
	FieldSignedBy      = "Signed-By"
)

// releaseFieldOrder is the order a generated Release file emits its fields in,
// matching Debian's own. Any field not listed (one preserved from a previously
// published Release) is emitted after these, in the order it was read.
var releaseFieldOrder = []string{
	FieldOrigin, FieldLabel, FieldSuite, FieldReleaseVer, FieldCodename,
	FieldDate, FieldValidUntil, FieldAcquireByHash,
	FieldNotAutomatic, FieldButAutoUpg, FieldSignedBy,
	FieldArchitectures, FieldComponents, FieldReleaseDesc,
}

// DateLayout is the timestamp format a Release file uses: RFC 1123 in UTC, the
// only form every apt version parses.
const DateLayout = "Mon, 02 Jan 2006 15:04:05 UTC"

// IndexFile is one entry in a Release file's checksum blocks: an index the
// Release covers, its size, and its digest under each algorithm in use.
type IndexFile struct {
	// Path is relative to the suite directory, e.g. "main/binary-amd64/Packages".
	Path   string
	Size   int64
	Hashes Hashes
}

// Release is a parsed (or to-be-written) suite Release file: its descriptive
// fields, plus the set of indexes it covers.
type Release struct {
	// Para holds the descriptive fields. The checksum blocks are held
	// separately in Files and are stripped from Para on parse, so a round trip
	// does not duplicate them.
	Para *Paragraph
	// Files are the indexes the Release covers, ordered by path.
	Files []IndexFile
}

// NewRelease returns an empty Release.
func NewRelease() *Release { return &Release{Para: NewParagraph()} }

// Get reads a descriptive field.
func (r *Release) Get(name string) string { return r.Para.Get(name) }

// Set writes a descriptive field.
func (r *Release) Set(name, value string) { r.Para.Set(name, value) }

// Suite returns the suite the Release describes.
func (r *Release) Suite() string { return r.Para.Get(FieldSuite) }

// Architectures returns the architectures the Release lists.
func (r *Release) Architectures() []string { return SplitList(r.Para.Get(FieldArchitectures)) }

// Components returns the components the Release lists.
func (r *Release) Components() []string { return SplitList(r.Para.Get(FieldComponents)) }

// AcquireByHash reports whether the Release advertises by-hash index paths.
func (r *Release) AcquireByHash() bool {
	return strings.EqualFold(strings.TrimSpace(r.Para.Get(FieldAcquireByHash)), "yes")
}

// FileByPath returns the entry for an index path, or nil.
func (r *Release) FileByPath(p string) *IndexFile {
	for i := range r.Files {
		if r.Files[i].Path == p {
			return &r.Files[i]
		}
	}
	return nil
}

// HashAlgorithms returns the algorithms the Release publishes checksum blocks
// for.
func (r *Release) HashAlgorithms() []string {
	seen := map[string]bool{}
	for _, f := range r.Files {
		for algo := range f.Hashes {
			seen[algo] = true
		}
	}
	return canonicalHashOrder(seen)
}

// ParseRelease reads a Release (or the body of an InRelease) document.
func ParseRelease(data []byte) (*Release, error) {
	paras, err := ParseParagraphs(data)
	if err != nil {
		return nil, fmt.Errorf("parse Release: %w", err)
	}
	if len(paras) == 0 {
		return nil, fmt.Errorf("parse Release: document is empty")
	}
	para := paras[0]

	rel := &Release{Para: NewParagraph()}
	byPath := map[string]*IndexFile{}
	for _, f := range para.Fields {
		algo, isBlock := hashForReleaseField(f.Name)
		if !isBlock {
			rel.Para.Set(f.Name, f.Value)
			continue
		}
		for _, line := range strings.Split(f.Value, "\n") {
			hash, size, p, ok := parseChecksumLine(line)
			if !ok {
				continue
			}
			entry := byPath[p]
			if entry == nil {
				entry = &IndexFile{Path: p, Size: size, Hashes: Hashes{}}
				byPath[p] = entry
			}
			entry.Hashes[algo] = hash
		}
	}
	for _, p := range sortedKeys(byPath) {
		rel.Files = append(rel.Files, *byPath[p])
	}
	return rel, nil
}

// hashForReleaseField maps a Release field name to the hash algorithm whose
// block it is, reporting false for a descriptive field.
func hashForReleaseField(name string) (string, bool) {
	switch strings.ToLower(name) {
	case "md5sum":
		return HashMD5, true
	case "sha1":
		return HashSHA1, true
	case "sha256":
		return HashSHA256, true
	default:
		return "", false
	}
}

// Render writes the Release document: the descriptive fields in canonical
// order, then one folded checksum block per algorithm.
func (r *Release) Render() []byte {
	out := NewParagraph()

	emitted := map[string]bool{}
	for _, name := range releaseFieldOrder {
		if r.Para.Has(name) {
			out.Set(name, r.Para.Get(name))
			emitted[strings.ToLower(name)] = true
		}
	}
	// Anything the repository was created with that this tool does not manage
	// (a vendor field, NotAutomatic variants we did not list) is preserved.
	for _, f := range r.Para.Fields {
		if emitted[strings.ToLower(f.Name)] {
			continue
		}
		if _, isBlock := hashForReleaseField(f.Name); isBlock {
			continue
		}
		out.Set(f.Name, f.Value)
	}

	files := append([]IndexFile(nil), r.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	var buf bytes.Buffer
	buf.Write(out.Render())
	for _, algo := range r.HashAlgorithms() {
		buf.WriteString(releaseBlock[algo])
		buf.WriteString(":\n")
		for _, f := range files {
			h, ok := f.Hashes[algo]
			if !ok {
				continue
			}
			fmt.Fprintf(&buf, " %s %16d %s\n", h, f.Size, f.Path)
		}
	}
	return buf.Bytes()
}

// SetDate records the Release timestamp, and optionally a Valid-Until a given
// duration later. A zero validity leaves Valid-Until unset, so the Release does
// not expire.
func (r *Release) SetDate(t time.Time, validity time.Duration) {
	t = t.UTC()
	r.Set(FieldDate, t.Format(DateLayout))
	if validity > 0 {
		r.Set(FieldValidUntil, t.Add(validity).UTC().Format(DateLayout))
	} else {
		r.Para.Delete(FieldValidUntil)
	}
}

// Date returns the Release timestamp, or the zero time if it is absent or
// unparseable.
func (r *Release) Date() time.Time {
	t, err := time.Parse(DateLayout, r.Get(FieldDate))
	if err != nil {
		return time.Time{}
	}
	return t
}

// ValidUntil returns the Release expiry, or the zero time if it does not expire.
func (r *Release) ValidUntil() time.Time {
	raw := r.Get(FieldValidUntil)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(DateLayout, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ComponentRelease builds the small per-component, per-architecture Release
// file that sits beside a Packages index. It is not required by apt, but it is
// conventional and some mirroring tools read it, so it is cheap to keep.
func ComponentRelease(suite *Release, component, arch string) []byte {
	p := NewParagraph()
	for _, name := range []string{FieldOrigin, FieldLabel} {
		if v := suite.Get(name); v != "" {
			p.Set(name, v)
		}
	}
	p.Set("Archive", suite.Get(FieldSuite))
	if v := suite.Get(FieldCodename); v != "" {
		p.Set(FieldCodename, v)
	}
	p.Set("Component", component)
	p.Set(FieldArchitecture, arch)
	return p.Render()
}
