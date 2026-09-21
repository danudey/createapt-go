package sign

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

// LoadKeyring reads one or more public keyring files, each either
// ASCII-armored (.asc) or binary (.gpg), and returns the combined entity list.
func LoadKeyring(paths ...string) (openpgp.EntityList, error) {
	var all openpgp.EntityList
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read keyring %s: %w", p, err)
		}
		entities, err := readKeyRing(data)
		if err != nil {
			return nil, fmt.Errorf("parse keyring %s: %w", p, err)
		}
		all = append(all, entities...)
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no public keys found in the supplied keyring(s)")
	}
	return all, nil
}

// VerifyDetached checks an armored detached signature (a Release.gpg) over
// data, returning the fingerprint of the key that made it.
func VerifyDetached(data, signature []byte, keyrings []string) (string, error) {
	kr, err := LoadKeyring(keyrings...)
	if err != nil {
		return "", err
	}
	signer, err := openpgp.CheckArmoredDetachedSignature(kr, bytes.NewReader(data), bytes.NewReader(signature), nil)
	if err != nil {
		// A binary (unarmored) signature is unusual for a Release.gpg but is
		// still valid, so it is worth a second attempt before giving up.
		signer, err = openpgp.CheckDetachedSignature(kr, bytes.NewReader(data), bytes.NewReader(signature), nil)
		if err != nil {
			return "", err
		}
	}
	return fingerprintOf(signer), nil
}

// VerifyClearsigned checks an inline-signed document (an InRelease), returning
// the signed body and the fingerprint of the key that made the signature.
//
// The body returned is the bytes the signature actually covers, so a caller
// that parses it is parsing what was signed rather than what the file appears
// to say.
func VerifyClearsigned(document []byte, keyrings []string) (body []byte, fingerprint string, err error) {
	kr, err := LoadKeyring(keyrings...)
	if err != nil {
		return nil, "", err
	}
	block, _ := clearsign.Decode(document)
	if block == nil {
		return nil, "", fmt.Errorf("the document is not an OpenPGP clearsigned message")
	}
	signer, err := openpgp.CheckDetachedSignature(kr, bytes.NewReader(block.Bytes), block.ArmoredSignature.Body, nil)
	if err != nil {
		return nil, "", err
	}
	return block.Plaintext, fingerprintOf(signer), nil
}

// fingerprintOf renders a verified signer's primary-key fingerprint.
func fingerprintOf(e *openpgp.Entity) string {
	if e == nil || e.PrimaryKey == nil {
		return "an unidentified key"
	}
	return strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint))
}

// ExportPublicKeyToFile exports keyID from the local GnuPG keyring into a
// temporary armored keyring file, returning its path and a cleanup function.
// It is how a copy verifies a source repository against the key its config
// records, without the caller having to supply --keyring.
func ExportPublicKeyToFile(keyID string) (path string, cleanup func(), err error) {
	noop := func() {}
	armored, err := ExportPublicKey(keyID)
	if err != nil {
		return "", noop, err
	}
	f, err := os.CreateTemp("", "createapt-key-*.asc")
	if err != nil {
		return "", noop, err
	}
	name := f.Name()
	remove := func() { _ = os.Remove(name) }
	if _, err := f.Write(armored); err != nil {
		f.Close()
		remove()
		return "", noop, err
	}
	if err := f.Close(); err != nil {
		remove()
		return "", noop, err
	}
	return name, remove, nil
}
