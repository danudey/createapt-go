package sign

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// Fingerprint resolves whichever of a key file or a keyring key id was supplied
// into the signing key's primary-key fingerprint.
//
// The fingerprint, rather than the short id or the uid that was typed, is what
// gets recorded in the repository config: it is unambiguous and portable across
// machines, so a later publish from a different host resolves the same key.
// Neither source supplied returns ("", nil).
func Fingerprint(keyFile, keyID string) (string, error) {
	switch {
	case keyFile != "":
		return fingerprintFromFile(keyFile)
	case keyID != "":
		return fingerprintFromKeyring(keyID)
	default:
		return "", nil
	}
}

// fingerprintFromFile reads the key file directly, so it works with no gpg
// installed and without importing anything into the user's keyring.
func fingerprintFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	entities, err := readKeyRing(data)
	if err != nil {
		return "", fmt.Errorf("read key %s: %w", path, err)
	}
	for _, e := range entities {
		if e.PrimaryKey != nil {
			return strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint)), nil
		}
	}
	return "", fmt.Errorf("no key found in %s", path)
}

// fingerprintFromKeyring asks gpg to resolve an id or uid against the local
// keyring. gpg's colon-delimited output is parsed rather than its human
// output, which is not stable across versions.
func fingerprintFromKeyring(keyID string) (string, error) {
	cmd := gpgCommand("--with-colons", "--fingerprint", keyID)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gpg --fingerprint %s: %w: %s", keyID, err, strings.TrimSpace(errBuf.String()))
	}
	// The first fpr: record after the first pub: record is the primary key's.
	seenPub := false
	for _, line := range strings.Split(out.String(), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "pub":
			seenPub = true
		case "fpr":
			if seenPub && len(fields) > 9 && fields[9] != "" {
				return strings.ToUpper(fields[9]), nil
			}
		}
	}
	return "", fmt.Errorf("gpg reported no fingerprint for %q", keyID)
}

// ExportPublicKey writes the armored public key for keyID out of the local
// keyring. It is what lets a repository publish the key its clients need to
// install alongside it.
func ExportPublicKey(keyID string) ([]byte, error) {
	cmd := gpgCommand("--armor", "--export", keyID)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gpg --export %s: %w: %s", keyID, err, strings.TrimSpace(errBuf.String()))
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("gpg exported no key for %q", keyID)
	}
	return out.Bytes(), nil
}
