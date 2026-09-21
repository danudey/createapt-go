//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// aptClient is an apt installation isolated from the host's: its own
// configuration, sources, trusted keys, lists and dpkg status. Pointing it at a
// generated repository is the test that matters, because apt is the only
// authority on whether the metadata this tool writes is correct.
type aptClient struct {
	t    *testing.T
	root string
	conf string
}

// newAptClient sets up an isolated apt, skipping the test if apt is absent.
func newAptClient(t *testing.T) *aptClient {
	t.Helper()
	requireTool(t, "apt-get", "apt is needed to read the generated repositories")

	root := t.TempDir()
	for _, d := range []string{
		"etc/apt/sources.list.d", "etc/apt/trusted.gpg.d", "etc/apt/preferences.d",
		"var/lib/apt/lists/partial", "var/cache/apt/archives/partial", "var/lib/dpkg",
	} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// An empty dpkg status file means "nothing installed", which is what makes
	// the client's view depend only on the repository under test.
	if err := os.WriteFile(filepath.Join(root, "var/lib/dpkg/status"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	conf := filepath.Join(root, "apt.conf")
	body := fmt.Sprintf(`Dir "%[1]s";
Dir::State "%[1]s/var/lib/apt";
Dir::State::status "%[1]s/var/lib/dpkg/status";
Dir::State::lists "%[1]s/var/lib/apt/lists";
Dir::Cache "%[1]s/var/cache/apt";
Dir::Etc "%[1]s/etc/apt";
Dir::Etc::sourcelist "%[1]s/etc/apt/sources.list.d/test.list";
Dir::Etc::sourceparts "/dev/null";
Dir::Etc::main "%[2]s";
Dir::Etc::parts "/dev/null";
Dir::Etc::preferences "/dev/null";
Dir::Etc::preferencesparts "/dev/null";
Dir::Etc::trustedparts "%[1]s/etc/apt/trusted.gpg.d";
APT::Architecture "amd64";
APT::Architectures "amd64";
`, root, conf)
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return &aptClient{t: t, root: root, conf: conf}
}

// Trust installs an armored public key so apt will accept the repository's
// signature.
func (a *aptClient) Trust(armoredKey []byte) {
	a.t.Helper()
	path := filepath.Join(a.root, "etc/apt/trusted.gpg.d", "createapt-test.asc")
	if err := os.WriteFile(path, armoredKey, 0o644); err != nil {
		a.t.Fatal(err)
	}
}

// Source points the client at one repository. options is the bracketed
// sources.list option group without the brackets, e.g. "trusted=yes".
func (a *aptClient) Source(uri, suite, component, options string) {
	a.t.Helper()
	opts := ""
	if options != "" {
		opts = "[" + options + "] "
	}
	line := fmt.Sprintf("deb %s%s %s %s\n", opts, uri, suite, component)
	path := filepath.Join(a.root, "etc/apt/sources.list.d", "test.list")
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		a.t.Fatal(err)
	}
	// A changed source invalidates whatever was fetched before.
	_ = os.RemoveAll(filepath.Join(a.root, "var/lib/apt/lists"))
	if err := os.MkdirAll(filepath.Join(a.root, "var/lib/apt/lists/partial"), 0o755); err != nil {
		a.t.Fatal(err)
	}
}

// SourceLine writes a complete sources.list line, for the cases that need one
// this helper does not otherwise compose (deb-src, several components).
func (a *aptClient) SourceLine(line string) {
	a.t.Helper()
	path := filepath.Join(a.root, "etc/apt/sources.list.d", "test.list")
	if err := os.WriteFile(path, []byte(strings.TrimRight(line, "\n")+"\n"), 0o644); err != nil {
		a.t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(a.root, "var/lib/apt/lists"))
	if err := os.MkdirAll(filepath.Join(a.root, "var/lib/apt/lists/partial"), 0o755); err != nil {
		a.t.Fatal(err)
	}
}

// run executes an apt command against the isolated configuration.
func (a *aptClient) run(name string, args ...string) (string, error) {
	a.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "APT_CONFIG="+a.conf, "DEBIAN_FRONTEND=noninteractive")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// Update runs apt-get update and fails the test if apt rejects the repository.
func (a *aptClient) Update() string {
	a.t.Helper()
	out, err := a.run("apt-get", "update")
	a.t.Logf("$ apt-get update\n%s", out)
	if err != nil {
		a.t.Fatalf("apt-get update failed: %v\n%s", err, out)
	}
	// apt reports a bad signature or a hash mismatch as a warning on stderr
	// while still exiting zero, so the output has to be inspected too.
	for _, bad := range []string{
		"Hash Sum mismatch",
		"is not signed",
		"NO_PUBKEY",
		"GPG error",
		"The repository ",
	} {
		if strings.Contains(out, bad) {
			a.t.Fatalf("apt-get update reported %q:\n%s", bad, out)
		}
	}
	return out
}

// UpdateExpectingFailure runs apt-get update and requires that apt rejects the
// repository, returning its output.
func (a *aptClient) UpdateExpectingFailure() string {
	a.t.Helper()
	out, err := a.run("apt-get", "update")
	a.t.Logf("$ apt-get update (expecting rejection)\n%s", out)
	rejected := err != nil ||
		strings.Contains(out, "is not signed") ||
		strings.Contains(out, "GPG error") ||
		strings.Contains(out, "NO_PUBKEY")
	if !rejected {
		a.t.Fatalf("apt accepted a repository it should have rejected:\n%s", out)
	}
	return out
}

// Candidate returns the version apt would install for a package, or "" when it
// does not know the package at all.
func (a *aptClient) Candidate(pkg string) string {
	a.t.Helper()
	out, err := a.run("apt-cache", "policy", pkg)
	if err != nil {
		a.t.Fatalf("apt-cache policy %s failed: %v\n%s", pkg, err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "Candidate:"); ok {
			v := strings.TrimSpace(after)
			if v == "(none)" {
				return ""
			}
			return v
		}
	}
	return ""
}

// Download fetches a package through apt into the client's archive directory,
// which exercises the checksums the indexes record.
func (a *aptClient) Download(pkg string) string {
	a.t.Helper()
	out, err := a.run("apt-get", "install", "--download-only", "--reinstall", "-y", pkg)
	a.t.Logf("$ apt-get install --download-only %s\n%s", pkg, out)
	if err != nil {
		a.t.Fatalf("apt could not download %s: %v\n%s", pkg, err, out)
	}
	if strings.Contains(out, "Hash Sum mismatch") || strings.Contains(out, "Size mismatch") {
		a.t.Fatalf("apt reported a mismatch downloading %s:\n%s", pkg, out)
	}
	return out
}

// SourceCandidate returns the version apt-cache reports for a source package,
// or "" when the Sources index does not describe it.
func (a *aptClient) SourceCandidate(pkg string) string {
	a.t.Helper()
	out, err := a.run("apt-cache", "showsrc", pkg)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if after, ok := strings.CutPrefix(line, "Version: "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}
