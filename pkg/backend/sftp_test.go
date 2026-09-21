package backend

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testHostKey returns a throwaway public key usable as a host key.
func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh public key: %v", err)
	}
	return sshPub
}

// writeKnownHosts points HOME at a temp dir containing the given known_hosts
// lines (or no known_hosts file at all when lines is empty).
func writeKnownHosts(t *testing.T, lines string) {
	t.Helper()
	home := t.TempDir()
	if lines != "" {
		sshDir := filepath.Join(home, ".ssh")
		if err := os.MkdirAll(sshDir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sshDir, "known_hosts"), []byte(lines), 0o600); err != nil {
			t.Fatalf("write known_hosts: %v", err)
		}
	}
	t.Setenv("HOME", home)
}

func TestHostKeyCallbackMissingKnownHosts(t *testing.T) {
	writeKnownHosts(t, "")
	if _, err := hostKeyCallback(); err == nil {
		t.Fatal("want an error when known_hosts is absent, got nil")
	} else if !strings.Contains(err.Error(), "--insecure-ignore-host-key") {
		t.Errorf("error should name the opt-out flag, got: %v", err)
	}
}

func TestHostKeyCallbackUnknownHost(t *testing.T) {
	key := testHostKey(t)
	// A known_hosts listing some other host, so the file reads but our host is absent.
	writeKnownHosts(t, "other.example.com "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))+"\n")

	cb, err := hostKeyCallback()
	if err != nil {
		t.Fatalf("hostKeyCallback: %v", err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	err = cb("host.example.com:22", addr, key)
	if err == nil {
		t.Fatal("want an error for a host missing from known_hosts, got nil")
	}
	if !strings.Contains(err.Error(), "not listed") {
		t.Errorf("error should say the host is not listed, got: %v", err)
	}
}

func TestHostKeyCallbackKnownHost(t *testing.T) {
	key := testHostKey(t)
	writeKnownHosts(t, "host.example.com "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))+"\n")

	cb, err := hostKeyCallback()
	if err != nil {
		t.Fatalf("hostKeyCallback: %v", err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	if err := cb("host.example.com:22", addr, key); err != nil {
		t.Errorf("listed host should verify, got: %v", err)
	}
}

func TestHostKeyCallbackInsecureOptIn(t *testing.T) {
	writeKnownHosts(t, "")
	InsecureIgnoreHostKey = true
	t.Cleanup(func() { InsecureIgnoreHostKey = false })

	cb, err := hostKeyCallback()
	if err != nil {
		t.Fatalf("hostKeyCallback: %v", err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	if err := cb("host.example.com:22", addr, testHostKey(t)); err != nil {
		t.Errorf("insecure callback should accept any key, got: %v", err)
	}
}
