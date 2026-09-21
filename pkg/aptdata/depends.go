package aptdata

import (
	"fmt"
	"strings"
)

// The relationship fields a binary package can carry. Only Depends and
// Pre-Depends are treated as hard requirements by the dependency checker;
// Recommends and Suggests are advisory and a missing one is not a broken
// repository.
const (
	FieldDepends    = "Depends"
	FieldPreDepends = "Pre-Depends"
	FieldRecommends = "Recommends"
	FieldSuggests   = "Suggests"
	FieldProvides   = "Provides"
	FieldConflicts  = "Conflicts"
	FieldBreaks     = "Breaks"
	FieldReplaces   = "Replaces"
	FieldEnhances   = "Enhances"
)

// HardDependencyFields are the fields whose relations must be satisfiable for
// a package to be installable.
var HardDependencyFields = []string{FieldDepends, FieldPreDepends}

// Relation is one alternative within a dependency: a package name, an optional
// architecture qualifier, and an optional version constraint.
type Relation struct {
	// Name is the (possibly virtual) package name depended on.
	Name string
	// ArchQualifier is the ":any"/":native"/":<arch>" suffix, without the colon.
	ArchQualifier string
	// Op is the comparison operator: "", "<<", "<=", "=", ">=" or ">>".
	Op string
	// Version is the version the operator compares against; empty when Op is.
	Version string
	// Arches restricts the relation to (or, with a leading "!", away from) the
	// listed architectures, as in "foo [amd64 !i386]".
	Arches []string
	// Profiles holds the build-profile restrictions, as in "foo <!nocheck>".
	// They apply to build dependencies only and are preserved, not interpreted.
	Profiles []string
}

// String renders the relation back to its control-field form.
func (r Relation) String() string {
	var b strings.Builder
	b.WriteString(r.Name)
	if r.ArchQualifier != "" {
		b.WriteString(":")
		b.WriteString(r.ArchQualifier)
	}
	if r.Op != "" {
		fmt.Fprintf(&b, " (%s %s)", r.Op, r.Version)
	}
	if len(r.Arches) > 0 {
		fmt.Fprintf(&b, " [%s]", strings.Join(r.Arches, " "))
	}
	for _, p := range r.Profiles {
		fmt.Fprintf(&b, " <%s>", p)
	}
	return b.String()
}

// Constraint renders just the constrained-name portion, for diagnostics.
func (r Relation) Constraint() string {
	if r.Op == "" {
		return r.Name
	}
	return fmt.Sprintf("%s (%s %s)", r.Name, r.Op, r.Version)
}

// Satisfies reports whether a package providing r.Name at the given version
// meets r's version constraint. An unversioned relation is satisfied by any
// version; a versioned relation against a provider with no version (a plain
// virtual package) is not, matching apt's rule that an unversioned Provides
// cannot satisfy a versioned dependency.
func (r Relation) Satisfies(version string) bool {
	if r.Op == "" {
		return true
	}
	if version == "" {
		return false
	}
	c := CompareVersions(version, r.Version)
	switch r.Op {
	case "<<":
		return c < 0
	case "<=", "<":
		return c <= 0
	case "=":
		return c == 0
	case ">=":
		return c >= 0
	case ">>", ">":
		return c > 0
	default:
		return false
	}
}

// AppliesTo reports whether the relation is in force for a package built for
// the given architecture. A relation with no architecture list always applies;
// one with a list applies when the architecture is included (or, for a negated
// list, not excluded).
func (r Relation) AppliesTo(arch string) bool {
	if len(r.Arches) == 0 {
		return true
	}
	negated := strings.HasPrefix(r.Arches[0], "!")
	for _, a := range r.Arches {
		name := strings.TrimPrefix(a, "!")
		if name == arch || name == "any" || (arch == ArchAll && name == "all") {
			return !negated
		}
	}
	return negated
}

// Alternatives is one comma-separated dependency item: a set of relations any
// one of which satisfies it ("foo | bar").
type Alternatives []Relation

// String renders the group back to its control-field form.
func (a Alternatives) String() string {
	parts := make([]string, len(a))
	for i, r := range a {
		parts[i] = r.String()
	}
	return strings.Join(parts, " | ")
}

// ParseRelations parses a whole relationship field into one Alternatives group
// per comma-separated item. An empty field yields no groups.
//
// Parsing is deliberately forgiving: a malformed item is returned as a bare
// name rather than an error, because refusing to index an otherwise valid
// package over an unparseable Suggests would be worse than ignoring it.
func ParseRelations(field string) []Alternatives {
	var out []Alternatives
	for _, item := range splitOutsideBrackets(field, ',') {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		var group Alternatives
		for _, alt := range splitOutsideBrackets(item, '|') {
			alt = strings.TrimSpace(alt)
			if alt == "" {
				continue
			}
			group = append(group, parseRelation(alt))
		}
		if len(group) > 0 {
			out = append(out, group)
		}
	}
	return out
}

// parseRelation parses a single alternative such as
// "libfoo:any (>= 1.2) [amd64 !i386] <!nocheck>".
func parseRelation(s string) Relation {
	var r Relation

	// Build profiles: zero or more <...> groups, always last.
	for {
		open := strings.LastIndex(s, "<")
		if open < 0 || !strings.HasSuffix(strings.TrimSpace(s), ">") {
			break
		}
		closeIdx := strings.Index(s[open:], ">")
		if closeIdx < 0 {
			break
		}
		r.Profiles = append([]string{s[open+1 : open+closeIdx]}, r.Profiles...)
		s = strings.TrimSpace(s[:open] + s[open+closeIdx+1:])
	}

	// Architecture restriction: a single [...] group.
	if open := strings.Index(s, "["); open >= 0 {
		if closeIdx := strings.Index(s[open:], "]"); closeIdx > 0 {
			r.Arches = strings.Fields(s[open+1 : open+closeIdx])
			s = strings.TrimSpace(s[:open] + s[open+closeIdx+1:])
		}
	}

	// Version constraint: a single (op version) group.
	if open := strings.Index(s, "("); open >= 0 {
		if closeIdx := strings.Index(s[open:], ")"); closeIdx > 0 {
			r.Op, r.Version = parseConstraint(s[open+1 : open+closeIdx])
			s = strings.TrimSpace(s[:open] + s[open+closeIdx+1:])
		}
	}

	// What remains is the name, optionally architecture-qualified.
	name := strings.TrimSpace(s)
	if i := strings.Index(name, ":"); i > 0 {
		r.Name, r.ArchQualifier = name[:i], name[i+1:]
	} else {
		r.Name = name
	}
	return r
}

// parseConstraint splits "(>= 1.2)"'s body into its operator and version.
func parseConstraint(body string) (op, version string) {
	body = strings.TrimSpace(body)
	// Longest operators first, so "<=" is not read as "<".
	for _, candidate := range []string{"<<", "<=", ">=", ">>", "=", "<", ">"} {
		if strings.HasPrefix(body, candidate) {
			return candidate, strings.TrimSpace(body[len(candidate):])
		}
	}
	// A bare version with no operator means equality in practice.
	return "=", body
}

// splitOutsideBrackets splits on sep, ignoring separators inside (), [] or <>.
// Debian relationship fields never nest those groups, so a depth counter over
// all three bracket kinds is sufficient.
func splitOutsideBrackets(s string, sep byte) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '<':
			depth++
		case ')', ']', '>':
			if depth > 0 {
				depth--
			}
		case sep:
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// Relations parses one of the package's relationship fields.
func (p *Package) Relations(field string) []Alternatives {
	return ParseRelations(p.Get(field))
}

// ProvidedCapabilities returns the package's own name plus everything its
// Provides field declares, each with the version it is provided at (empty for
// an unversioned Provides).
func (p *Package) ProvidedCapabilities() map[string]string {
	out := map[string]string{p.Name(): p.Version()}
	for _, group := range p.Relations(FieldProvides) {
		for _, r := range group {
			// A Provides entry may only use "=", and only when versioned.
			version := ""
			if r.Op == "=" {
				version = r.Version
			}
			// The package's own name, if re-declared, keeps its real version.
			if existing, ok := out[r.Name]; ok && existing != "" {
				continue
			}
			out[r.Name] = version
		}
	}
	return out
}
