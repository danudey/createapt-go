// Package sign provides the signing and verification an apt repository needs:
// signing a suite's Release file, and verifying a repository's Release against
// a trusted key.
//
// Debian has no per-package signature that apt checks. A .deb is trusted
// because the signed Release vouches for the Packages index, which records the
// package's sha256 — so signing the Release is what makes every package in the
// repository trusted, and there is nothing to sign on a package itself.
//
// Two key sources are supported: a private key file (handled natively with
// OpenPGP) and a key identifier from the user's GnuPG keyring (delegated to the
// gpg binary).
package sign

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

// ReleaseSigner signs a suite's Release file in the two forms apt reads: a
// detached armored signature (Release.gpg) and an inline clearsigned document
// (InRelease). It implements repo.Signer.
type ReleaseSigner struct {
	// entity is set for key-file signing (native).
	entity *openpgp.Entity
	// keyID is set for keyring signing (delegated to gpg).
	keyID string
	// passphrase unlocks the key when gpg needs one supplied non-interactively.
	passphrase string
}

// NewKeyFileSigner loads an OpenPGP private key from path (armored or binary)
// and returns a signer. If the key is passphrase-protected, passphrase is used
// to decrypt it.
func NewKeyFileSigner(path, passphrase string) (*ReleaseSigner, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	entities, err := readKeyRing(data)
	if err != nil {
		return nil, fmt.Errorf("read signing key %s: %w", path, err)
	}
	var signer *openpgp.Entity
	for _, e := range entities {
		if e.PrivateKey != nil {
			signer = e
			break
		}
	}
	if signer == nil {
		return nil, fmt.Errorf("no private key found in %s", path)
	}
	if signer.PrivateKey.Encrypted {
		if passphrase == "" {
			return nil, fmt.Errorf("signing key %s is encrypted but no passphrase was provided", path)
		}
		if err := decryptEntity(signer, []byte(passphrase)); err != nil {
			return nil, fmt.Errorf("decrypt signing key: %w", err)
		}
	}
	return &ReleaseSigner{entity: signer}, nil
}

// NewKeyIDSigner returns a signer that delegates to gpg, using the given key id
// (or any gpg-recognized user-id/fingerprint) from the local keyring.
func NewKeyIDSigner(keyID, passphrase string) *ReleaseSigner {
	return &ReleaseSigner{keyID: keyID, passphrase: passphrase}
}

// SignDetached returns an ASCII-armored detached signature over data, which is
// written as the suite's Release.gpg.
func (s *ReleaseSigner) SignDetached(data []byte) ([]byte, error) {
	if s.entity != nil {
		var buf bytes.Buffer
		if err := openpgp.ArmoredDetachSign(&buf, s.entity, bytes.NewReader(data), nil); err != nil {
			return nil, err
		}
		// apt requires the armored signature to end in a newline.
		return ensureTrailingNewline(buf.Bytes()), nil
	}
	return s.gpg(data, "--armor", "--detach-sign")
}

// SignClearsigned wraps data in an OpenPGP clearsigned document, which is
// written as the suite's InRelease.
func (s *ReleaseSigner) SignClearsigned(data []byte) ([]byte, error) {
	if s.entity != nil {
		var buf bytes.Buffer
		w, err := clearsign.Encode(&buf, s.entity.PrivateKey, nil)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return ensureTrailingNewline(buf.Bytes()), nil
	}
	return s.gpg(data, "--clearsign")
}

// gpg shells out to the gpg binary to sign data with the configured key.
func (s *ReleaseSigner) gpg(data []byte, mode ...string) ([]byte, error) {
	args := append([]string{"--batch", "--yes", "--output", "-"}, mode...)
	if s.keyID != "" {
		args = append(args, "--local-user", s.keyID)
	}
	if s.passphrase != "" {
		// --pinentry-mode loopback is what lets a passphrase be supplied
		// without a terminal, which is the case in any automated publish.
		args = append(args, "--pinentry-mode", "loopback", "--passphrase-fd", "0")
	}

	cmd := exec.Command(GPGBinary(), args...)
	if s.passphrase != "" {
		cmd.Stdin = strings.NewReader(s.passphrase + "\n")
		// The document to sign cannot also come from stdin, so it is passed by
		// file instead.
		tmp, cleanup, err := writeTemp(data)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		cmd.Args = append(cmd.Args, tmp)
	} else {
		cmd.Stdin = bytes.NewReader(data)
	}

	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gpg %s: %w: %s", strings.Join(mode, " "), err, errBuf.String())
	}
	return ensureTrailingNewline(out.Bytes()), nil
}

// writeTemp spills data to a temporary file, returning its path and a cleanup.
func writeTemp(data []byte) (string, func(), error) {
	f, err := os.CreateTemp("", "createapt-release-*")
	if err != nil {
		return "", func() {}, err
	}
	name := f.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return name, cleanup, nil
}

// GPGBinary returns the gpg command to invoke, overridable for testing and for
// hosts where it is not on the default path.
func GPGBinary() string {
	if g := os.Getenv("CREATEAPT_GPG"); g != "" {
		return g
	}
	return "gpg"
}

func ensureTrailingNewline(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] != '\n' {
		return append(b, '\n')
	}
	return b
}

// readKeyRing reads OpenPGP entities, accepting either armored or binary input.
func readKeyRing(data []byte) (openpgp.EntityList, error) {
	if block, err := armor.Decode(bytes.NewReader(data)); err == nil {
		return openpgp.ReadKeyRing(block.Body)
	}
	return openpgp.ReadKeyRing(bytes.NewReader(data))
}

// decryptEntity decrypts a private key and its subkeys in place.
func decryptEntity(e *openpgp.Entity, passphrase []byte) error {
	if e.PrivateKey != nil && e.PrivateKey.Encrypted {
		if err := e.PrivateKey.Decrypt(passphrase); err != nil {
			return err
		}
	}
	for _, sk := range e.Subkeys {
		if sk.PrivateKey != nil && sk.PrivateKey.Encrypted {
			if err := sk.PrivateKey.Decrypt(passphrase); err != nil {
				return err
			}
		}
	}
	return nil
}
