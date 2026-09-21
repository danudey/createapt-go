package repocheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/danudey/createapt-go/pkg/backend"
	"github.com/danudey/createapt-go/pkg/debmeta"
	"github.com/danudey/createapt-go/pkg/progress"
)

// errDebToolUnavailable reports that archive verification was skipped because
// the tooling it needs is not installed. It is distinguished from a real
// failure so a missing tool never marks a repository broken.
var errDebToolUnavailable = errors.New("no archive verifier available")

// DebTool verifies that a downloaded .deb is a well-formed archive, beyond the
// size and checksum the index records.
//
// The check is done in process: the ar container is walked and the control
// stanza is read out of the control tarball, which exercises the same code path
// a client's dpkg would. That means it needs nothing installed, so unlike the
// rpm tool this mirrors, it is always available — the unavailable case is kept
// only so a future external verifier can report it.
type DebTool struct {
	// Enabled reports whether archive verification runs at all.
	Enabled bool
}

// DetectDebTool returns the archive verifier. It takes no arguments and never
// fails, because the verification is implemented natively.
func DetectDebTool() DebTool { return DebTool{Enabled: true} }

// Describe reports what the tool will do, for the run's header.
func (d DebTool) Describe() string {
	if !d.Enabled {
		return "archive verification disabled"
	}
	return "archives are parsed in process (no dpkg required)"
}

// Verify opens the .deb at path and reads its control stanza, which fails if
// the ar container, the compressed control tarball, or the control file itself
// is malformed.
func (d DebTool) Verify(path string) error {
	if !d.Enabled {
		return errDebToolUnavailable
	}
	para, err := debmeta.ControlFromDeb(path)
	if err != nil {
		return err
	}
	if para.Get("Package") == "" {
		return fmt.Errorf("the archive's control file names no package")
	}
	return nil
}

// readAll reads an object from a backend fully into memory.
func readAll(ctx context.Context, be backend.Backend, relpath string) ([]byte, error) {
	rc, err := be.Get(ctx, relpath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// fetchToTempFile streams an object into a temporary file, returning its path,
// size and sha256. The caller owns the file. task, when non-nil, is credited
// with the bytes as they arrive.
func fetchToTempFile(ctx context.Context, be backend.Backend, relpath string, task *progress.Task) (local string, size int64, sum string, err error) {
	rc, err := be.Get(ctx, relpath)
	if err != nil {
		return "", 0, "", err
	}
	defer rc.Close()

	f, err := os.CreateTemp("", "createapt-check-*-"+path.Base(relpath))
	if err != nil {
		return "", 0, "", err
	}
	name := f.Name()

	h := sha256.New()
	size, err = io.Copy(task.Writer(io.MultiWriter(f, h)), rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(name)
		return "", 0, "", err
	}
	return name, size, hex.EncodeToString(h.Sum(nil)), nil
}

func removeFile(p string) { _ = os.Remove(p) }
