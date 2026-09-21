//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// signingKey is a throwaway GnuPG key in its own GNUPGHOME, used to sign the
// repositories the suite publishes and to give apt something to trust.
type signingKey struct {
	UID         string
	Fingerprint string
	Public      []byte
	home        string
}

// newSigningKey generates a key, skipping the test if gpg is not installed.
// The GNUPGHOME it creates is set for the whole test, so the CLI under test
// finds the key too.
func newSigningKey(t *testing.T) *signingKey {
	t.Helper()
	requireTool(t, "gpg", "signing cannot be exercised without it")

	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GNUPGHOME", home)

	const uid = "createapt e2e <e2e@example.invalid>"
	gen := exec.Command("gpg", "--batch", "--passphrase", "", "--quick-generate-key",
		uid, "rsa3072", "sign", "never")
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("could not generate a signing key with gpg: %v\n%s", err, out)
	}

	k := &signingKey{UID: uid, home: home}
	k.Fingerprint = k.fingerprint(t)
	k.Public = k.export(t)
	return k
}

func (k *signingKey) fingerprint(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("gpg", "--batch", "--with-colons", "--fingerprint", k.UID).Output()
	if err != nil {
		t.Fatalf("gpg --fingerprint: %v", err)
	}
	seenPub := false
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "pub":
			seenPub = true
		case "fpr":
			if seenPub && len(fields) > 9 {
				return fields[9]
			}
		}
	}
	t.Fatal("gpg reported no fingerprint for the generated key")
	return ""
}

func (k *signingKey) export(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	cmd := exec.Command("gpg", "--batch", "--armor", "--export", k.UID)
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("gpg --export: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("gpg exported an empty key")
	}
	return buf.Bytes()
}

// KeyringFile writes the public key to a file for --keyring.
func (k *signingKey) KeyringFile(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/public.asc"
	if err := os.WriteFile(path, k.Public, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
