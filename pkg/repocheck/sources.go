package repocheck

import (
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/danudey/createapt-go/pkg/aptdata"
)

// The two entry types a sources file can name.
const (
	typeBinary = "deb"
	typeSource = "deb-src"
)

// SourceEntry is one repository a client would subscribe to: a base URI, a
// suite, and the components within it. It is what a sources.list line or a
// deb822 .sources stanza describes.
type SourceEntry struct {
	// URI is the repository root the suite lives under.
	URI string
	// Suite is the dists/ subdirectory.
	Suite string
	// Components are the sections named; empty means every component the
	// Release lists.
	Components []string
	// Arches are the architectures named in the entry's options, if any.
	Arches []string
	// Source reports whether the entry is a deb-src line rather than a deb one.
	Source bool
}

// Label renders the entry the way a sources.list line does, for reporting.
func (e SourceEntry) Label() string {
	kind := typeBinary
	if e.Source {
		kind = typeSource
	}
	parts := []string{kind, e.URI, e.Suite}
	parts = append(parts, e.Components...)
	return strings.Join(parts, " ")
}

// IsSourcesFile reports whether a path names a sources file rather than a
// repository location.
func IsSourcesFile(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".list", ".sources":
		return true
	default:
		return false
	}
}

// LoadSources reads a one-line-per-entry sources.list (.list) or a deb822
// .sources file and returns the entries it describes. fetch is used when input
// is an http(s) URL.
func LoadSources(input string, fetch func(string) ([]byte, error)) ([]SourceEntry, []string, error) {
	var data []byte
	var err error
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		data, err = fetch(input)
	} else {
		data, err = os.ReadFile(input)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", input, err)
	}

	if strings.EqualFold(path.Ext(input), ".sources") {
		return parseDeb822Sources(data)
	}
	return parseListSources(data)
}

// parseListSources reads the one-line format:
//
//	deb [arch=amd64 signed-by=...] https://example.com/apt bookworm main contrib
func parseListSources(data []byte) ([]SourceEntry, []string, error) {
	var entries []SourceEntry
	var warnings []string

	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entry, err := parseListLine(line)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("line %d: %v", i+1, err))
			continue
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil, warnings, fmt.Errorf("no usable deb/deb-src entries found")
	}
	return entries, warnings, nil
}

// parseListLine parses a single sources.list line.
func parseListLine(line string) (SourceEntry, error) {
	var e SourceEntry

	fields := strings.Fields(line)
	if len(fields) == 0 {
		return e, fmt.Errorf("empty entry")
	}
	switch fields[0] {
	case typeBinary:
	case typeSource:
		e.Source = true
	default:
		return e, fmt.Errorf("entry does not start with deb or deb-src")
	}
	fields = fields[1:]

	// An options group "[k=v k=v]" may follow the type. Only arch is
	// interpreted; the rest (signed-by, trusted) describe how a client should
	// treat the repository, not what it contains.
	if len(fields) > 0 && strings.HasPrefix(fields[0], "[") {
		var opts []string
		for len(fields) > 0 {
			opts = append(opts, strings.Trim(fields[0], "[]"))
			closed := strings.HasSuffix(fields[0], "]")
			fields = fields[1:]
			if closed {
				break
			}
		}
		for _, o := range opts {
			if k, v, ok := strings.Cut(o, "="); ok && strings.EqualFold(k, "arch") {
				e.Arches = aptdata.SplitList(v)
			}
		}
	}

	if len(fields) < 2 {
		return e, fmt.Errorf("entry names no suite")
	}
	e.URI, e.Suite = fields[0], fields[1]
	e.Components = fields[2:]

	// A suite ending in "/" is the trivial ("flat") layout, which has no
	// dists/ tree and which this tool does not produce or validate.
	if strings.HasSuffix(e.Suite, "/") {
		return e, fmt.Errorf("entry uses the trivial (flat) repository layout, which is not supported")
	}
	if len(e.Components) == 0 {
		return e, fmt.Errorf("entry names no components")
	}
	return e, nil
}

// parseDeb822Sources reads the deb822 .sources format, in which one stanza can
// describe several suites and components at once.
func parseDeb822Sources(data []byte) ([]SourceEntry, []string, error) {
	paras, err := aptdata.ParseParagraphs(data)
	if err != nil {
		return nil, nil, fmt.Errorf("parse .sources: %w", err)
	}

	var entries []SourceEntry
	var warnings []string
	for i, p := range paras {
		if strings.EqualFold(strings.TrimSpace(p.Get("Enabled")), "no") {
			continue
		}
		types := aptdata.SplitList(p.Get("Types"))
		uris := aptdata.SplitList(p.Get("URIs"))
		suites := aptdata.SplitList(p.Get("Suites"))
		components := aptdata.SplitList(p.Get("Components"))
		arches := aptdata.SplitList(p.Get("Architectures"))

		if len(types) == 0 || len(uris) == 0 || len(suites) == 0 {
			warnings = append(warnings, fmt.Sprintf("stanza %d: missing Types, URIs or Suites", i+1))
			continue
		}
		if len(components) == 0 {
			warnings = append(warnings, fmt.Sprintf("stanza %d: names no Components", i+1))
			continue
		}
		for _, t := range types {
			isSource := t == typeSource
			if t != typeBinary && !isSource {
				warnings = append(warnings, fmt.Sprintf("stanza %d: unknown type %q", i+1, t))
				continue
			}
			for _, uri := range uris {
				for _, suite := range suites {
					entries = append(entries, SourceEntry{
						URI: uri, Suite: suite, Components: components,
						Arches: arches, Source: isSource,
					})
				}
			}
		}
	}
	if len(entries) == 0 {
		return nil, warnings, fmt.Errorf("no enabled entries found in the .sources file")
	}
	return entries, warnings, nil
}
