//go:build e2e

// Package e2e drives the built createapt-go binary end to end: it publishes
// repositories to every backend the tool supports and then reads them back with
// a real apt, which is the only way to prove the generated metadata is what apt
// actually accepts.
//
// Every optional dependency skips itself rather than failing, so the suite is
// useful on a developer machine with only dpkg-deb installed and complete on
// one with apt, gpg, docker and an ssh server.
package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// binary builds the CLI once per run and returns its path.
var binary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "createapt-e2e-bin-")
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "createapt-go")

	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	cmd := exec.Command("go", "build", "-o", out, "./cmd/createapt-go")
	cmd.Dir = root
	if combined, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building the CLI: %w: %s", err, combined)
	}
	return out, nil
})

// repoRoot locates the module root from the test's working directory.
func repoRoot() (string, error) {
	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", fmt.Errorf("go.mod not found at %s: %w", dir, err)
	}
	return dir, nil
}

// packagesDir returns the sample packages built by reference/gen.sh, skipping
// the test if they have not been generated.
func packagesDir(t *testing.T) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "reference", "packages")
	if _, err := os.Stat(filepath.Join(dir, "hello_2.10-3_all.deb")); err != nil {
		t.Skipf("reference packages are not built; run reference/gen.sh (%v)", err)
	}
	return dir
}

func pkgPath(t *testing.T, name string) string {
	return filepath.Join(packagesDir(t), name)
}

// cli runs the built binary, failing the test on a non-zero exit.
func cli(t *testing.T, args ...string) string {
	t.Helper()
	out, err := tryCLI(t, args...)
	if err != nil {
		t.Fatalf("createapt-go %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// tryCLI runs the built binary and returns its combined output and error, for
// the cases where a failure is the expected outcome.
func tryCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return tryCLIRaw(t, withSigningIntent(args)...)
}

// tryCLIRaw is tryCLI with the arguments passed through exactly as given, for
// the tests that are about what the CLI does with no signing flags at all.
func tryCLIRaw(t *testing.T, args ...string) (string, error) {
	t.Helper()
	bin, err := binary()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "CREATEAPT_AWS_CACHE=off")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	t.Logf("$ createapt-go %s\n%s", strings.Join(args, " "), buf.String())
	return buf.String(), err
}

// publishingCommands are the subcommands that write a repository, and so have
// to state whether what they publish is signed.
var publishingCommands = map[string]bool{
	"create": true, "add": true, "remove": true, "rebuild": true, "copy": true,
}

// withSigningIntent appends --no-sign-release to a publishing command that
// names no signing key. Signing is the tool's default, so a test that is not
// about signatures would otherwise be refused; the tests that do exercise
// signing pass a key and are left alone. TestUnsignedPublishingIsRefused covers
// the refusal itself.
func withSigningIntent(args []string) []string {
	if len(args) == 0 || !publishingCommands[args[0]] {
		return args
	}
	for _, a := range args {
		switch {
		case a == "--no-sign-release", a == "--sign-release",
			strings.HasPrefix(a, "--gpg-key"), strings.HasPrefix(a, "--no-sign-release="):
			return args
		}
	}
	return append(append([]string(nil), args...), "--no-sign-release")
}

// have reports whether a command is on the path.
func have(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// requireTool skips the test unless the named command is installed.
func requireTool(t *testing.T, name, why string) {
	t.Helper()
	if !have(name) {
		t.Skipf("%s is not installed; %s", name, why)
	}
}
