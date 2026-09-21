package repocheck

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danudey/createapt-go/pkg/backend"
	"github.com/danudey/createapt-go/pkg/repo"
	"github.com/danudey/createapt-go/pkg/repoconfig"
)

// publish builds a small repository to validate.
func publish(t *testing.T, suite string, names ...string) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	packages, err := filepath.Abs(filepath.Join("..", "..", "reference", "packages"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(packages); err != nil {
		t.Skipf("reference packages are not built; run reference/gen.sh (%v)", err)
	}

	r, err := repo.Open(ctx, dir, repo.Options{Suite: suite, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, name := range names {
		if _, err := r.Add(filepath.Join(packages, name)); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	if _, err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return dir
}

func run(t *testing.T, opts Options) []Result {
	t.Helper()
	if opts.Deb.Enabled == false {
		opts.Deb = DetectDebTool()
	}
	results, warnings, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v (warnings: %v)", err, warnings)
	}
	return results
}

func counts(results []Result) (ok, fail int) {
	o, f, _, _ := Summarize(results)
	return o, f
}

func TestCheckPassesOnAHealthyRepository(t *testing.T) {
	dir := publish(t, "bookworm", "hello_2.10-3_all.deb", "libfoo_1.3.0-1_amd64.deb")

	for _, level := range []Level{LevelMetadata, LevelHead, LevelFetch} {
		results := run(t, Options{
			Input: dir, Suites: []string{"bookworm"}, Arches: []string{"any"},
			Level: level, Concurrency: 2,
		})
		ok, fail := counts(results)
		if fail != 0 {
			t.Errorf("level %s reported %d failures: %v", level, fail, failures(results))
		}
		if ok == 0 {
			t.Errorf("level %s checked nothing", level)
		}
	}
}

func TestCheckDetectsAReplacedPackage(t *testing.T) {
	dir := publish(t, "bookworm", "hello_2.10-3_all.deb")

	// Publish a different build over the indexed one. This is the failure the
	// command exists to catch: the indexes still describe the old file.
	packages, _ := filepath.Abs(filepath.Join("..", "..", "reference", "packages"))
	replacement, err := os.ReadFile(filepath.Join(packages, "libfoo_1.3.0-1_amd64.deb"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "pool", "main", "h", "hello", "hello_2.10-3_all.deb")
	if err := os.WriteFile(target, replacement, 0o644); err != nil {
		t.Fatal(err)
	}

	results := run(t, Options{Input: dir, Suites: []string{"bookworm"}, Arches: []string{"any"}, Level: LevelHead})
	_, fail := counts(results)
	if fail == 0 {
		t.Fatal("a replaced package was not detected")
	}
	if !strings.Contains(failures(results), "size") {
		t.Errorf("the failure does not mention the size mismatch: %s", failures(results))
	}
}

func TestCheckDetectsACorruptedIndex(t *testing.T) {
	dir := publish(t, "bookworm", "hello_2.10-3_all.deb")

	index := filepath.Join(dir, "dists", "bookworm", "main", "binary-all", "Packages")
	if err := os.WriteFile(index, []byte("Package: tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	results := run(t, Options{Input: dir, Suites: []string{"bookworm"}, Level: LevelMetadata})
	_, fail := counts(results)
	if fail == 0 {
		t.Fatal("an index that no longer matches the Release was accepted")
	}
}

func TestCheckReportsAMissingSuite(t *testing.T) {
	dir := publish(t, "bookworm", "hello_2.10-3_all.deb")
	results := run(t, Options{Input: dir, Suites: []string{"trixie"}, Level: LevelMetadata})
	_, fail := counts(results)
	if fail != 1 {
		t.Errorf("got %d failures for a suite that does not exist; want 1", fail)
	}
	if !strings.Contains(failures(results), "neither InRelease nor Release") {
		t.Errorf("unhelpful failure for a missing suite: %s", failures(results))
	}
}

func TestCheckUsesTheRecordedSuiteWhenNoneIsNamed(t *testing.T) {
	ctx := context.Background()
	dir := publish(t, "jammy", "hello_2.10-3_all.deb")

	// Record the suite the way the CLI does, then check with no --suite.
	be, err := openBackendFor(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeConfigSuite(ctx, be, "jammy"); err != nil {
		t.Fatal(err)
	}

	results := run(t, Options{Input: dir, Level: LevelMetadata})
	if _, fail := counts(results); fail != 0 {
		t.Errorf("checking with no --suite did not find the recorded suite: %v", failures(results))
	}
}

func TestParseListSources(t *testing.T) {
	doc := `# a comment
deb [arch=amd64 signed-by=/etc/apt/keyrings/x.asc] https://deb.example.com/apt bookworm main contrib
deb-src https://deb.example.com/apt bookworm main

deb https://deb.example.com/apt flat/
`
	entries, warnings, err := parseListSources([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries; want 2: %+v", len(entries), entries)
	}
	if entries[0].URI != "https://deb.example.com/apt" || entries[0].Suite != "bookworm" {
		t.Errorf("first entry = %+v", entries[0])
	}
	if strings.Join(entries[0].Components, ",") != "main,contrib" {
		t.Errorf("components = %v; want main and contrib", entries[0].Components)
	}
	if strings.Join(entries[0].Arches, ",") != "amd64" {
		t.Errorf("arches = %v; want amd64 from the options group", entries[0].Arches)
	}
	if !entries[1].Source {
		t.Error("the deb-src entry was not marked as a source entry")
	}
	// The trivial (flat) layout is refused, with a warning naming the line.
	if len(warnings) != 1 || !strings.Contains(warnings[0], "flat") {
		t.Errorf("warnings = %v; want one about the flat layout", warnings)
	}
}

func TestParseDeb822Sources(t *testing.T) {
	doc := `Types: deb deb-src
URIs: https://deb.example.com/apt
Suites: bookworm trixie
Components: main
Architectures: amd64 arm64

Types: deb
URIs: https://other.example.com/apt
Suites: stable
Components: main
Enabled: no
`
	entries, _, err := parseDeb822Sources([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	// Two types times two suites, and the disabled stanza is skipped.
	if len(entries) != 4 {
		t.Fatalf("got %d entries; want 4: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.URI != "https://deb.example.com/apt" {
			t.Errorf("the disabled stanza was not skipped: %+v", e)
		}
	}
}

func TestCheckFromASourcesFile(t *testing.T) {
	dir := publish(t, "bookworm", "hello_2.10-3_all.deb")

	listPath := filepath.Join(t.TempDir(), "test.list")
	line := "deb file://" + dir + " bookworm main\n"
	if err := os.WriteFile(listPath, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	results := run(t, Options{Input: listPath, Arches: []string{"any"}, Level: LevelHead})
	if _, fail := counts(results); fail != 0 {
		t.Errorf("checking through a sources.list failed: %v", failures(results))
	}
	if len(results) == 0 {
		t.Error("checking through a sources.list checked nothing")
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]Level{
		"metadata": LevelMetadata, "meta": LevelMetadata,
		"head": LevelHead, "fetch": LevelFetch, "FETCH": LevelFetch,
	} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("deep"); err == nil {
		t.Error("an unknown level was accepted")
	}
}

// failures renders the failing results for an error message.
func failures(results []Result) string {
	var out []string
	for _, r := range results {
		if r.Status == StatusFail {
			out = append(out, r.Kind+": "+r.Detail)
		}
	}
	return strings.Join(out, "; ")
}

// openBackendFor and writeConfigSuite record a suite in a repository's config
// the way the CLI does, so the "no --suite given" path can be exercised.
func openBackendFor(dir string) (backend.Backend, error) {
	return backend.Open(context.Background(), dir)
}

func writeConfigSuite(ctx context.Context, be backend.Backend, suite string) error {
	return repoconfig.Save(ctx, be, &repoconfig.Config{Suite: suite})
}
