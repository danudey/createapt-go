package aptdata

import (
	"strings"
	"testing"
)

// pkg builds a minimal binary package stanza for the dependency tests.
func pkg(t *testing.T, name, version, arch string, fields map[string]string) *Package {
	t.Helper()
	p := NewPackage(NewParagraph())
	p.Set(FieldPackage, name)
	p.Set(FieldVersion, version)
	p.Set(FieldArchitecture, arch)
	for k, v := range fields {
		p.Set(k, v)
	}
	// The identity is the .deb's checksum; the tests only need it to be unique.
	p.Set(FieldSHA256, name+"_"+version+"_"+arch)
	p.Component = "main"
	return p
}

func TestParseRelations(t *testing.T) {
	groups := ParseRelations("libfoo (>= 1.3.0) | libbar, baz [amd64 !i386] <!nocheck>, qux:any")
	if len(groups) != 3 {
		t.Fatalf("got %d groups; want 3", len(groups))
	}

	if len(groups[0]) != 2 {
		t.Fatalf("first group has %d alternatives; want 2", len(groups[0]))
	}
	if r := groups[0][0]; r.Name != "libfoo" || r.Op != ">=" || r.Version != "1.3.0" {
		t.Errorf("first alternative = %+v; want libfoo >= 1.3.0", r)
	}
	if r := groups[0][1]; r.Name != "libbar" || r.Op != "" {
		t.Errorf("second alternative = %+v; want an unversioned libbar", r)
	}

	baz := groups[1][0]
	if baz.Name != "baz" {
		t.Errorf("name = %q; want baz", baz.Name)
	}
	if strings.Join(baz.Arches, " ") != "amd64 !i386" {
		t.Errorf("arches = %v; want [amd64 !i386]", baz.Arches)
	}
	if strings.Join(baz.Profiles, " ") != "!nocheck" {
		t.Errorf("profiles = %v; want [!nocheck]", baz.Profiles)
	}

	if r := groups[2][0]; r.Name != "qux" || r.ArchQualifier != "any" {
		t.Errorf("third group = %+v; want qux:any", r)
	}
}

func TestRelationSatisfies(t *testing.T) {
	tests := []struct {
		relation string
		version  string
		want     bool
	}{
		{"foo (>= 1.0)", "1.0", true},
		{"foo (>= 1.0)", "0.9", false},
		{"foo (>> 1.0)", "1.0", false},
		{"foo (<< 2.0)", "1.9", true},
		{"foo (= 1.0-1)", "1.0-1", true},
		{"foo (= 1.0-1)", "1.0-2", false},
		// An unversioned dependency is met by any version.
		{"foo", "0.1", true},
		// A versioned dependency is not met by an unversioned Provides, which
		// is apt's rule and the reason a virtual package cannot stand in for a
		// specific version.
		{"foo (>= 1.0)", "", false},
	}
	for _, tc := range tests {
		r := ParseRelations(tc.relation)[0][0]
		if got := r.Satisfies(tc.version); got != tc.want {
			t.Errorf("%q satisfied by version %q = %v; want %v", tc.relation, tc.version, got, tc.want)
		}
	}
}

func TestRelationAppliesTo(t *testing.T) {
	inclusive := ParseRelations("foo [amd64 arm64]")[0][0]
	if !inclusive.AppliesTo("amd64") || inclusive.AppliesTo("i386") {
		t.Error("an inclusive architecture list did not gate the relation correctly")
	}
	negated := ParseRelations("foo [!i386]")[0][0]
	if !negated.AppliesTo("amd64") || negated.AppliesTo("i386") {
		t.Error("a negated architecture list did not gate the relation correctly")
	}
	plain := ParseRelations("foo")[0][0]
	if !plain.AppliesTo("anything") {
		t.Error("a relation with no architecture list should always apply")
	}
}

func TestCheckDependencies(t *testing.T) {
	app := pkg(t, "app", "1.0-1", "amd64", map[string]string{FieldDepends: "deplib (= 1.0-1), libc6 (>= 2.36)"})
	dep := pkg(t, "deplib", "1.0-1", "amd64", nil)

	// With the exact version present, only libc6 is unmet — and libc6 is
	// external (nothing in the repository provides it), so it is not reported.
	if problems := CheckDependencies([]*Package{app, dep}); len(problems) != 0 {
		t.Errorf("got %v; want no problems", problems)
	}

	// Replacing deplib with a newer version leaves app's pinned dependency
	// unmet, and this time the repository does provide the name, so it counts.
	newer := pkg(t, "deplib", "2.0-1", "amd64", nil)
	problems := CheckDependencies([]*Package{app, newer})
	if len(problems) != 1 {
		t.Fatalf("got %d problems; want 1: %v", len(problems), problems)
	}
	if problems[0].Package.Name() != "app" {
		t.Errorf("problem reported against %q; want app", problems[0].Package.Name())
	}
}

func TestCheckDependenciesUsesProvides(t *testing.T) {
	app := pkg(t, "app", "1.0-1", "amd64", map[string]string{FieldDepends: "virtual (>= 1.0)"})
	provider := pkg(t, "real", "3.0-1", "amd64", map[string]string{FieldProvides: "virtual (= 1.5)"})

	if problems := CheckDependencies([]*Package{app, provider}); len(problems) != 0 {
		t.Errorf("a versioned Provides did not satisfy the dependency: %v", problems)
	}

	// An unversioned Provides cannot satisfy a versioned dependency.
	unversioned := pkg(t, "real", "3.0-1", "amd64", map[string]string{FieldProvides: "virtual"})
	if problems := CheckDependencies([]*Package{app, unversioned}); len(problems) != 1 {
		t.Errorf("an unversioned Provides satisfied a versioned dependency: %v", problems)
	}
}

func TestCheckDependenciesAlternativesAndReportedOnce(t *testing.T) {
	// Either alternative satisfies the group.
	app := pkg(t, "app", "1.0-1", "amd64", map[string]string{FieldDepends: "missing | present"})
	present := pkg(t, "present", "1.0-1", "amd64", nil)
	if problems := CheckDependencies([]*Package{app, present}); len(problems) != 0 {
		t.Errorf("an alternative that was satisfiable was reported: %v", problems)
	}

	// An arch:all package in the same repository must not cause an
	// architecture-specific package's problem to be reported once per
	// architecture set.
	broken := pkg(t, "broken", "1.0-1", "amd64", map[string]string{FieldDepends: "present (>= 9.0)"})
	anyArch := pkg(t, "docs", "1.0-1", ArchAll, nil)
	problems := CheckDependencies([]*Package{broken, present, anyArch})
	if len(problems) != 1 {
		t.Errorf("got %d problems; want exactly 1: %v", len(problems), problems)
	}
}

func TestRemovalBreakages(t *testing.T) {
	pinned1 := pkg(t, "pinned", "1.0-1", "amd64", nil)
	pinned2 := pkg(t, "pinned", "2.0-1", "amd64", nil)
	pinner := pkg(t, "pinner", "1.0-1", "amd64", map[string]string{FieldDepends: "pinned (= 1.0-1)"})
	all := []*Package{pinned1, pinned2, pinner}

	breakages := RemovalBreakages(all, []*Package{pinned1})
	if len(breakages) != 1 {
		t.Fatalf("got %d breakages; want 1: %v", len(breakages), breakages)
	}
	if breakages[0].Provider.Version() != "1.0-1" || breakages[0].Dependent.Name() != "pinner" {
		t.Errorf("breakage = %s; want pinned 1.0-1 required by pinner", breakages[0])
	}

	// Removing the version nothing pins breaks nothing.
	if breakages := RemovalBreakages(all, []*Package{pinned2}); len(breakages) != 0 {
		t.Errorf("removing an unpinned version reported %v", breakages)
	}
}
