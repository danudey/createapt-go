//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestProgressReportsWithoutDisturbingStdout publishes the same packages twice,
// once with --progress and once without, and compares the two runs.
//
// Two things have to hold for the flag to be safe to leave on: the progress
// display goes to stderr only, so a run whose stdout is consumed by something
// else is unaffected, and off a terminal it writes plain lines rather than
// escape sequences, so a CI log stays readable.
func TestProgressReportsWithoutDisturbingStdout(t *testing.T) {
	pkgs := packagesDir(t)
	args := func(repo string, extra ...string) []string {
		return append([]string{
			"add", repo,
			filepath.Join(pkgs, "hello_2.10-3_all.deb"),
			filepath.Join(pkgs, "libfoo_1.3.0-1_amd64.deb"),
			"--no-sign-release",
		}, extra...)
	}

	plainOut, plainErr := runSplit(t, args(filepath.Join(t.TempDir(), "plain"))...)
	progOut, progErr := runSplit(t, args(filepath.Join(t.TempDir(), "progress"), "--progress")...)

	if progOut != plainOut {
		t.Errorf("--progress changed the command's stdout:\nwithout:\n%s\nwith:\n%s", plainOut, progOut)
	}
	if strings.Contains(progErr, "\x1b[") {
		t.Errorf("--progress wrote terminal escape sequences to a pipe:\n%q", progErr)
	}
	if !strings.Contains(progErr, "publishing:") {
		t.Errorf("--progress reported no progress on stderr:\n%s", progErr)
	}
	if strings.Contains(plainErr, "publishing:") {
		t.Errorf("progress was reported without --progress:\n%s", plainErr)
	}
}

// runSplit runs the built binary with stdout and stderr captured separately,
// which the shared helper deliberately does not do.
func runSplit(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	bin, err := binary()
	if err != nil {
		t.Fatal(err)
	}
	var outBuf, errBuf bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "CREATEAPT_AWS_CACHE=off")
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("createapt-go %s failed: %v\n%s%s", strings.Join(args, " "), err, outBuf.String(), errBuf.String())
	}
	t.Logf("$ createapt-go %s\n%s%s", strings.Join(args, " "), outBuf.String(), errBuf.String())
	return outBuf.String(), errBuf.String()
}
