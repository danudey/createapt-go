package sign

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// testKey generates a throwaway signing key and writes it out as an armored
// private key file and an armored public keyring, returning both paths.
func testKey(t *testing.T) (privatePath, publicPath string) {
	t.Helper()
	entity, err := openpgp.NewEntity("createapt test", "signing", "test@example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	privatePath = filepath.Join(dir, "signing.asc")
	publicPath = filepath.Join(dir, "public.asc")

	write := func(path, blockType string, fn func(w io.WriteCloser) error) {
		var buf bytes.Buffer
		w, err := armor.Encode(&buf, blockType, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := fn(w); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(privatePath, openpgp.PrivateKeyType, func(w io.WriteCloser) error {
		return entity.SerializePrivateWithoutSigning(w, nil)
	})
	write(publicPath, openpgp.PublicKeyType, func(w io.WriteCloser) error {
		return entity.Serialize(w)
	})
	return privatePath, publicPath
}

func TestSignDetachedRoundTrip(t *testing.T) {
	private, public := testKey(t)
	signer, err := NewKeyFileSigner(private, "")
	if err != nil {
		t.Fatal(err)
	}

	release := []byte("Origin: Example\nSuite: bookworm\n")
	sig, err := signer.SignDetached(release)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(sig, []byte("-----BEGIN PGP SIGNATURE-----")) {
		t.Errorf("the detached signature is not ASCII-armored: %q", sig[:40])
	}
	// apt requires the armored signature to end in a newline.
	if sig[len(sig)-1] != '\n' {
		t.Error("the detached signature does not end in a newline")
	}

	fpr, err := VerifyDetached(release, sig, []string{public})
	if err != nil {
		t.Fatalf("a signature this tool made did not verify: %v", err)
	}
	if len(fpr) != 40 {
		t.Errorf("fingerprint = %q; want 40 hex characters", fpr)
	}

	// A signature must not verify against altered content.
	if _, err := VerifyDetached([]byte("Origin: Elsewhere\n"), sig, []string{public}); err == nil {
		t.Error("the signature verified against content it does not cover")
	}
}

func TestSignClearsignedRoundTrip(t *testing.T) {
	private, public := testKey(t)
	signer, err := NewKeyFileSigner(private, "")
	if err != nil {
		t.Fatal(err)
	}

	release := []byte("Origin: Example\nSuite: bookworm\nComponents: main\n")
	inRelease, err := signer.SignClearsigned(release)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(inRelease, []byte("-----BEGIN PGP SIGNED MESSAGE-----")) {
		t.Errorf("InRelease is not a clearsigned document: %q", inRelease[:40])
	}

	body, fpr, err := VerifyClearsigned(inRelease, []string{public})
	if err != nil {
		t.Fatalf("a clearsigned document this tool made did not verify: %v", err)
	}
	if fpr == "" {
		t.Error("no signer fingerprint was reported")
	}
	// The body returned must be what was signed, so a caller parses the signed
	// bytes rather than whatever the file appears to say.
	if strings.TrimRight(string(body), "\n") != strings.TrimRight(string(release), "\n") {
		t.Errorf("the verified body is %q; want the original Release", body)
	}

	// Tampering with the document must break verification.
	tampered := bytes.Replace(inRelease, []byte("Suite: bookworm"), []byte("Suite: trixie!"), 1)
	if _, _, err := VerifyClearsigned(tampered, []string{public}); err == nil {
		t.Error("a tampered clearsigned document verified")
	}
}

func TestFingerprintFromFile(t *testing.T) {
	private, _ := testKey(t)
	fpr, err := Fingerprint(private, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fpr) != 40 || fpr != strings.ToUpper(fpr) {
		t.Errorf("Fingerprint = %q; want a 40-character uppercase hex fingerprint", fpr)
	}

	// Neither source supplied is not an error; it simply has no answer.
	if got, err := Fingerprint("", ""); err != nil || got != "" {
		t.Errorf("Fingerprint(\"\", \"\") = %q, %v; want an empty result and no error", got, err)
	}
}

func TestNewKeyFileSignerRejectsAPublicKey(t *testing.T) {
	_, public := testKey(t)
	if _, err := NewKeyFileSigner(public, ""); err == nil {
		t.Fatal("a public keyring was accepted as a signing key")
	}
}

func TestLoadKeyringRejectsNonKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-key.asc")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(path); err == nil {
		t.Error("a file that is not a keyring was accepted")
	}
}

// fakeGPG installs a stand-in gpg that records how it was invoked, so the
// non-interactive hardening can be asserted without a real gpg or a real key.
func fakeGPG(t *testing.T) (record func() (args string, env string)) {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	envFile := filepath.Join(dir, "env")
	script := filepath.Join(dir, "fake-gpg")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" > " + argsFile + "\n" +
		"printf 'GPG_TTY=%s DISPLAY=%s\\n' \"${GPG_TTY-unset}\" \"${DISPLAY-unset}\" > " + envFile + "\n" +
		"printf 'signature\\n'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREATEAPT_GPG", script)
	return func() (string, string) {
		t.Helper()
		args, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatal(err)
		}
		env, err := os.ReadFile(envFile)
		if err != nil {
			t.Fatal(err)
		}
		return string(args), string(env)
	}
}

// setNoPassphrasePrompt flips the package-wide switch for one test.
func setNoPassphrasePrompt(t *testing.T, v bool) {
	t.Helper()
	prev := NoPassphrasePrompt
	NoPassphrasePrompt = v
	t.Cleanup(func() { NoPassphrasePrompt = prev })
}

// TestNoPassphrasePromptHardensGPG covers the CI case: a runner with a TTY
// looks interactive to gpg-agent, so a missing passphrase would hang the job on
// a pinentry prompt. Nothing must be left for pinentry to use.
func TestNoPassphrasePromptHardensGPG(t *testing.T) {
	record := fakeGPG(t)
	t.Setenv("GPG_TTY", "/dev/tty")
	t.Setenv("DISPLAY", ":0")
	setNoPassphrasePrompt(t, true)

	if _, err := NewKeyIDSigner("releases@example.invalid", "").SignDetached([]byte("Origin: Example\n")); err != nil {
		t.Fatal(err)
	}
	args, env := record()

	for _, want := range []string{"--batch", "--no-tty", "--pinentry-mode error"} {
		if !strings.Contains(args, want) {
			t.Errorf("gpg was invoked without %s: %s", want, args)
		}
	}
	if strings.Contains(env, "GPG_TTY=/dev/tty") || strings.Contains(env, "DISPLAY=:0") {
		t.Errorf("gpg inherited a terminal or display pinentry could prompt on: %s", env)
	}
}

// A supplied passphrase is already non-interactive: loopback with the
// passphrase on stdin must be kept, not replaced by the erroring mode.
func TestNoPassphrasePromptKeepsLoopbackWhenAPassphraseIsGiven(t *testing.T) {
	record := fakeGPG(t)
	setNoPassphrasePrompt(t, true)

	if _, err := NewKeyIDSigner("releases@example.invalid", "hunter2").SignClearsigned([]byte("Origin: Example\n")); err != nil {
		t.Fatal(err)
	}
	args, _ := record()

	if !strings.Contains(args, "--pinentry-mode loopback") || !strings.Contains(args, "--passphrase-fd 0") {
		t.Errorf("a supplied passphrase is no longer passed through loopback: %s", args)
	}
	if strings.Contains(args, "--pinentry-mode error") {
		t.Errorf("the erroring pinentry mode overrode the supplied passphrase: %s", args)
	}
}

// Left off, prompting stays possible: an interactive user signing with an
// agent-held key must not be denied the terminal.
func TestPromptingIsAllowedByDefault(t *testing.T) {
	record := fakeGPG(t)
	setNoPassphrasePrompt(t, false)

	if _, err := NewKeyIDSigner("releases@example.invalid", "").SignDetached([]byte("Origin: Example\n")); err != nil {
		t.Fatal(err)
	}
	args, _ := record()

	if strings.Contains(args, "--no-tty") || strings.Contains(args, "--pinentry-mode") {
		t.Errorf("prompting was suppressed without being asked for: %s", args)
	}
}

// TestGPGSignerAgreesWithGPG signs through the gpg binary and verifies the
// result natively, which is what proves the two signing paths are
// interchangeable.
func TestGPGSignerAgreesWithGPG(t *testing.T) {
	if _, err := exec.LookPath(GPGBinary()); err != nil {
		t.Skip("gpg is not installed")
	}

	home := t.TempDir()
	t.Setenv("GNUPGHOME", home)
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	gen := exec.Command(GPGBinary(), "--batch", "--passphrase", "", "--quick-generate-key",
		"createapt test <keyring@example.invalid>", "rsa3072", "sign", "never")
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("could not generate a test key with gpg: %v: %s", err, out)
	}

	publicPath := filepath.Join(home, "public.asc")
	armored, err := ExportPublicKey("keyring@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, armored, 0o600); err != nil {
		t.Fatal(err)
	}

	release := []byte("Origin: Example\nSuite: bookworm\n")
	signer := NewKeyIDSigner("keyring@example.invalid", "")

	sig, err := signer.SignDetached(release)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyDetached(release, sig, []string{publicPath}); err != nil {
		t.Errorf("a gpg-made detached signature did not verify natively: %v", err)
	}

	inRelease, err := signer.SignClearsigned(release)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyClearsigned(inRelease, []string{publicPath}); err != nil {
		t.Errorf("a gpg-made clearsigned document did not verify natively: %v", err)
	}

	// The fingerprint resolved from the keyring must be the one that signed.
	fpr, err := Fingerprint("", "keyring@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	gotFpr, err := VerifyDetached(release, sig, []string{publicPath})
	if err != nil {
		t.Fatal(err)
	}
	if fpr != gotFpr {
		t.Errorf("Fingerprint reports %s but the signature was made by %s", fpr, gotFpr)
	}

	// Forbidding prompts must not break a key that needs no passphrase: real
	// gpg has to accept the hardened invocation, not just the fake one.
	setNoPassphrasePrompt(t, true)
	if _, err := signer.SignDetached(release); err != nil {
		t.Errorf("gpg rejected the non-interactive invocation: %v", err)
	}
	if _, err := Fingerprint("", "keyring@example.invalid"); err != nil {
		t.Errorf("gpg rejected the non-interactive --fingerprint invocation: %v", err)
	}
}
