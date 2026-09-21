package aptdata

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Kind classifies a package by the role it plays, so a copy can take (or leave)
// a whole class of them without naming each one.
type Kind string

// The kinds a repository's entries fall into.
const (
	// KindBinary is an ordinary .deb.
	KindBinary Kind = "binary"
	// KindSource is a source package (.dsc plus its tarballs).
	KindSource Kind = "source"
	// KindUdeb is a micro-deb used by the Debian installer. It is a separate
	// kind because a normal repository has no use for one.
	KindUdeb Kind = "udeb"
	// KindDebug is a detached-debug-symbols package. Debian names these
	// <pkg>-dbgsym (and historically <pkg>-dbg) and files them under the
	// "debug" section; they are usually many times the size of the package
	// they belong to, which is why they are worth excluding on their own.
	KindDebug Kind = "debug"
)

// ParseKinds turns a comma-separated list into a set, accepting "debuginfo"
// and "dbgsym" as aliases for "debug" so a command line written for an rpm
// repository still means what it says.
func ParseKinds(s string) (map[Kind]bool, error) {
	out := map[Kind]bool{}
	for _, part := range strings.Split(s, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		switch name {
		case "debuginfo", "debugsource", "dbgsym", "dbg":
			name = string(KindDebug)
		case "deb":
			name = string(KindBinary)
		case "src":
			name = string(KindSource)
		}
		k := Kind(name)
		switch k {
		case KindBinary, KindSource, KindUdeb, KindDebug:
			out[k] = true
		default:
			return nil, fmt.Errorf("unknown kind %q (valid: binary, source, udeb, debug)", part)
		}
	}
	return out, nil
}

// KindOf classifies an entry. A package is only one kind: a debug package is
// reported as debug rather than binary, so excluding debug packages does not
// also need binary to be excluded.
func KindOf(e Entry) Kind {
	src, isSource := e.(*Source)
	if isSource {
		_ = src
		return KindSource
	}
	p, ok := e.(*Package)
	if !ok {
		return KindBinary
	}
	if strings.EqualFold(p.Get(FieldSection), "debug") ||
		strings.HasSuffix(p.Name(), "-dbgsym") || strings.HasSuffix(p.Name(), "-dbg") {
		return KindDebug
	}
	if strings.EqualFold(path.Ext(p.Location()), ".udeb") {
		return KindUdeb
	}
	return KindBinary
}

// Filter selects a subset of a repository's entries.
type Filter struct {
	// Include, when non-empty, keeps only entries matching one of the patterns.
	Include []string
	// Exclude drops entries matching any of the patterns. It is applied after
	// Include, so an exclusion always wins.
	Exclude []string

	// Arches, when non-empty, keeps only these architectures. An arch:all
	// package is kept alongside any architecture, because a repository for one
	// architecture still needs them.
	Arches []string

	// Kinds, when non-empty, keeps only these kinds.
	Kinds map[Kind]bool
	// ExcludeKinds drops these kinds. It is applied after Kinds.
	ExcludeKinds map[Kind]bool

	// LatestOnly keeps only the newest version of each name+architecture.
	LatestOnly bool
}

// Empty reports whether the filter would keep everything.
func (f Filter) Empty() bool {
	return len(f.Include) == 0 && len(f.Exclude) == 0 && len(f.Arches) == 0 &&
		len(f.Kinds) == 0 && len(f.ExcludeKinds) == 0 && !f.LatestOnly
}

// Describe names the filter's active parts, for reporting why a copy is
// rebuilding rather than replicating.
func (f Filter) Describe() []string {
	var out []string
	if len(f.Include) > 0 {
		out = append(out, "--include "+strings.Join(f.Include, ","))
	}
	if len(f.Exclude) > 0 {
		out = append(out, "--exclude "+strings.Join(f.Exclude, ","))
	}
	if len(f.Arches) > 0 {
		out = append(out, "--arch "+strings.Join(f.Arches, ","))
	}
	if len(f.Kinds) > 0 {
		out = append(out, "--kinds "+joinKinds(f.Kinds))
	}
	if len(f.ExcludeKinds) > 0 {
		out = append(out, "--exclude-kinds "+joinKinds(f.ExcludeKinds))
	}
	if f.LatestOnly {
		out = append(out, "--latest-only")
	}
	return out
}

func joinKinds(m map[Kind]bool) string {
	var names []string
	for k := range m {
		names = append(names, string(k))
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// Apply returns the entries the filter keeps, in the input's order.
//
// It reports an architecture that was asked for but is absent, because a copy
// that silently produced an empty repository for a mistyped architecture would
// be worse than one that stopped.
func (f Filter) Apply(entries []Entry) ([]Entry, error) {
	if err := f.checkArches(entries); err != nil {
		return nil, err
	}

	kept := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if !f.keeps(e) {
			continue
		}
		kept = append(kept, e)
	}
	if f.LatestOnly {
		kept = latestOnly(kept)
	}
	return kept, nil
}

// checkArches rejects an architecture no entry has.
func (f Filter) checkArches(entries []Entry) error {
	if len(f.Arches) == 0 {
		return nil
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Arch()] = true
	}
	for _, a := range f.Arches {
		if !present[a] {
			return fmt.Errorf("no package in the source repository has architecture %q (present: %s)",
				a, strings.Join(sortedKeys(present), ", "))
		}
	}
	return nil
}

// keeps decides one entry.
func (f Filter) keeps(e Entry) bool {
	if len(f.Arches) > 0 && !f.archMatches(e) {
		return false
	}
	kind := KindOf(e)
	if len(f.Kinds) > 0 && !f.Kinds[kind] {
		return false
	}
	if f.ExcludeKinds[kind] {
		return false
	}
	if len(f.Include) > 0 && !matchesAny(e, f.Include) {
		return false
	}
	if matchesAny(e, f.Exclude) {
		return false
	}
	return true
}

// archMatches keeps an entry whose architecture was asked for. An arch:all
// package is kept alongside any requested architecture; a source package is
// kept only when "source" was named.
func (f Filter) archMatches(e Entry) bool {
	arch := e.Arch()
	if arch == ArchAll {
		// Only meaningful alongside a real architecture: a request for source
		// packages alone should not drag in every arch:all binary.
		for _, a := range f.Arches {
			if a != ArchSource {
				return true
			}
		}
		return false
	}
	for _, a := range f.Arches {
		if a == arch {
			return true
		}
	}
	return false
}

// matchesAny tests an entry against a list of shell globs. Each pattern is
// matched against the package name and against its name_version and
// name_version_arch forms, so "hello", "hello_2.10*" and
// "libfoo_1.2.0-1_amd64" all select what a reader would expect.
func matchesAny(e Entry, patterns []string) bool {
	candidates := []string{
		e.Name(),
		e.Name() + "_" + e.Version(),
		e.ID3(),
	}
	for _, pattern := range patterns {
		for _, c := range candidates {
			if ok, err := path.Match(pattern, c); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// latestOnly keeps the newest version of each name+architecture.
func latestOnly(entries []Entry) []Entry {
	best := map[string]Entry{}
	for _, e := range entries {
		cur, seen := best[e.Key()]
		if !seen || cur.ParsedVersion().Compare(e.ParsedVersion()) < 0 {
			best[e.Key()] = e
		}
	}
	kept := make([]Entry, 0, len(best))
	for _, e := range entries {
		if best[e.Key()] == e {
			kept = append(kept, e)
		}
	}
	return kept
}
