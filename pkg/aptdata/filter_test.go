package aptdata

import (
	"strings"
	"testing"
)

// entries builds a small mixed package set for the filter tests.
func entries(t *testing.T) []Entry {
	t.Helper()
	src := NewSource(NewParagraph())
	src.Set(FieldPackage, "hello")
	src.Set(FieldVersion, "2.10-3")
	src.SetDirectory("pool/main/h/hello")
	src.SetSourceFiles([]SourceFile{{Name: "hello_2.10-3.dsc", Size: 1, SHA256: "src-dsc"}})
	src.Component = "main"

	return []Entry{
		pkg(t, "hello", "2.10-3", "amd64", nil),
		pkg(t, "hello", "2.11-1", "amd64", nil),
		pkg(t, "hello", "2.11-1", "arm64", nil),
		pkg(t, "hello-doc", "2.11-1", ArchAll, nil),
		pkg(t, "hello-dbgsym", "2.11-1", "amd64", map[string]string{FieldSection: "debug"}),
		src,
	}
}

func names(es []Entry) string {
	var out []string
	for _, e := range es {
		out = append(out, e.ID3())
	}
	return strings.Join(out, " ")
}

func TestFilterLatestOnly(t *testing.T) {
	got, err := Filter{LatestOnly: true}.Apply(entries(t))
	if err != nil {
		t.Fatal(err)
	}
	// hello 2.10-3 amd64 is superseded by 2.11-1; every other name+arch slot
	// has a single version, so it survives.
	if strings.Contains(names(got), "hello_2.10-3_amd64") {
		t.Errorf("--latest-only kept a superseded version: %s", names(got))
	}
	if !strings.Contains(names(got), "hello_2.11-1_amd64") {
		t.Errorf("--latest-only dropped the newest version: %s", names(got))
	}
	// The source package is in its own name+arch slot and must survive.
	if !strings.Contains(names(got), "hello_2.10-3") {
		t.Errorf("--latest-only dropped the source package: %s", names(got))
	}
}

func TestFilterArchKeepsAllAndRejectsAbsent(t *testing.T) {
	got, err := Filter{Arches: []string{"amd64"}}.Apply(entries(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(names(got), "hello-doc_2.11-1_all") {
		t.Errorf("--arch amd64 dropped the arch:all package: %s", names(got))
	}
	if strings.Contains(names(got), "arm64") {
		t.Errorf("--arch amd64 kept an arm64 package: %s", names(got))
	}
	if strings.Contains(names(got), "hello_2.10-3 ") {
		t.Errorf("--arch amd64 kept the source package: %s", names(got))
	}

	// An architecture nothing has is a mistake worth stopping for, not an
	// empty result to puzzle over.
	absent := Filter{Arches: []string{"riscv64"}}
	if _, err := absent.Apply(entries(t)); err == nil {
		t.Error("an absent architecture was accepted")
	}
}

func TestFilterKinds(t *testing.T) {
	kinds, err := ParseKinds("debug,source")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Filter{ExcludeKinds: kinds}.Apply(entries(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(names(got), "dbgsym") {
		t.Errorf("--exclude-kinds debug kept a debug package: %s", names(got))
	}
	if len(got) != 4 {
		t.Errorf("got %d entries; want 4 (the binaries only): %s", len(got), names(got))
	}

	// "debuginfo" is accepted as an alias, so a command line written for an rpm
	// repository still means what it says.
	aliased, err := ParseKinds("debuginfo")
	if err != nil {
		t.Fatal(err)
	}
	if !aliased[KindDebug] {
		t.Error("debuginfo did not map to the debug kind")
	}
	if _, err := ParseKinds("nonsense"); err == nil {
		t.Error("an unknown kind was accepted")
	}
}

func TestFilterIncludeExcludeGlobs(t *testing.T) {
	got, err := Filter{Include: []string{"hello-*"}}.Apply(entries(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(names(got), "hello_2") {
		t.Errorf("--include hello-* matched the bare name hello: %s", names(got))
	}
	if !strings.Contains(names(got), "hello-doc") {
		t.Errorf("--include hello-* missed hello-doc: %s", names(got))
	}

	// An exclusion always wins over an inclusion.
	got, err = Filter{Include: []string{"hello*"}, Exclude: []string{"*dbgsym*"}}.Apply(entries(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(names(got), "dbgsym") {
		t.Errorf("--exclude did not override --include: %s", names(got))
	}

	// A full name_version_arch pattern selects exactly one build.
	got, err = Filter{Include: []string{"hello_2.11-1_arm64"}}.Apply(entries(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID3() != "hello_2.11-1_arm64" {
		t.Errorf("an exact name_version_arch pattern selected %s", names(got))
	}
}

func TestFilterEmptyAndDescribe(t *testing.T) {
	var zero Filter
	if !zero.Empty() {
		t.Error("a zero Filter is not reported as empty")
	}
	f := Filter{LatestOnly: true, Arches: []string{"amd64"}}
	if f.Empty() {
		t.Error("a Filter with settings is reported as empty")
	}
	desc := strings.Join(f.Describe(), " ")
	if !strings.Contains(desc, "--latest-only") || !strings.Contains(desc, "--arch amd64") {
		t.Errorf("Describe() = %q; want it to name both settings", desc)
	}
}

func TestKindOf(t *testing.T) {
	if got := KindOf(pkg(t, "foo-dbgsym", "1.0", "amd64", nil)); got != KindDebug {
		t.Errorf("a -dbgsym package classified as %q; want debug", got)
	}
	if got := KindOf(pkg(t, "foo", "1.0", "amd64", map[string]string{FieldSection: "debug"})); got != KindDebug {
		t.Errorf("a Section: debug package classified as %q; want debug", got)
	}
	if got := KindOf(pkg(t, "foo", "1.0", "amd64", nil)); got != KindBinary {
		t.Errorf("an ordinary package classified as %q; want binary", got)
	}
}
