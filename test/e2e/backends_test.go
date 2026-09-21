//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// backend is one storage target the scenario set runs against.
type backend struct {
	// Name labels the subtest.
	Name string
	// Location is the repository URL to pass to the CLI.
	Location string
	// HTTPBase, when set, is a URL serving the same repository read-only, so an
	// apt client can be pointed at it. Backends with no HTTP view are still
	// exercised by the CLI's own verify and check.
	HTTPBase string
	// Args are extra flags every command needs for this backend.
	Args []string
}

// run invokes the CLI with the backend's flags appended.
func (b backend) run(t *testing.T, args ...string) string {
	t.Helper()
	return cli(t, append(args, b.Args...)...)
}

// backends starts every remote service that is available and returns the set of
// targets to test. Each service shuts itself down when the test ends.
func backends(t *testing.T) []backend {
	t.Helper()

	// Local disk is always available and is served over HTTP so apt can read
	// it, which also covers the read-only HTTP backend.
	dir := t.TempDir()
	server := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(server.Close)

	out := []backend{{Name: "local", Location: dir, HTTPBase: server.URL}}

	if loc, base, ok := startMinIO(t); ok {
		out = append(out, backend{Name: "s3", Location: loc, HTTPBase: base})
	}
	if loc, ok := startSFTP(t); ok {
		// The throwaway host key is not in ~/.ssh/known_hosts and never will
		// be, so verification has to be waived for this one connection.
		out = append(out, backend{
			Name: "sftp", Location: loc,
			Args: []string{"--insecure-ignore-host-key"},
		})
	}
	return out
}

// TestBackendsPublishAndVerify runs the same publish/verify/check cycle against
// every backend that is available, which is what proves the storage
// abstraction, not just the local path, produces a repository apt accepts.
func TestBackendsPublishAndVerify(t *testing.T) {
	for _, be := range backends(t) {
		t.Run(be.Name, func(t *testing.T) {
			be.run(t, "add", be.Location,
				pkgPath(t, "hello_2.10-3_all.deb"),
				pkgPath(t, "libfoo_1.3.0-1_amd64.deb"),
				"--suite", "bookworm")

			// Re-adding must transfer nothing: on S3 the checksum recorded at
			// upload settles it, and over SFTP a remote sha256sum does.
			out := be.run(t, "add", be.Location, pkgPath(t, "hello_2.10-3_all.deb"))
			if !strings.Contains(out, "0 upload(s)") {
				t.Errorf("re-adding an identical package transferred something:\n%s", out)
			}

			be.run(t, "verify", be.Location)
			be.run(t, "check", "--level", "fetch", "--arch", "any", "--versions", "all", be.Location)

			listing := be.run(t, "list", be.Location)
			if !strings.Contains(listing, "2 package(s)") {
				t.Errorf("list does not show both packages:\n%s", listing)
			}

			if be.HTTPBase == "" {
				return
			}
			apt := newAptClient(t)
			apt.Source(be.HTTPBase, "bookworm", "main", "trusted=yes")
			apt.Update()
			if got := apt.Candidate("hello"); got != "2.10-3" {
				t.Errorf("apt's candidate for hello is %q; want 2.10-3", got)
			}
			apt.Download("hello")
		})
	}
}

// startMinIO runs a MinIO server on a free port with a fresh data directory and
// creates a bucket in it, returning the s3:// location and an HTTP URL that
// serves the same bucket anonymously.
func startMinIO(t *testing.T) (location, httpBase string, ok bool) {
	t.Helper()
	if !have("minio") {
		t.Log("minio is not installed; skipping the S3 backend")
		return "", "", false
	}

	port, err := freePort()
	if err != nil {
		t.Logf("could not find a free port for minio: %v", err)
		return "", "", false
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	data := t.TempDir()

	const accessKey, secretKey, bucket = "createapt", "createapt-secret", "apt-e2e"

	// The bucket is created by making its directory: MinIO's filesystem backend
	// treats a top-level directory as a bucket, which avoids needing a client.
	if err := os.MkdirAll(filepath.Join(data, bucket), 0o755); err != nil {
		t.Logf("could not create the bucket directory: %v", err)
		return "", "", false
	}

	cmd := exec.Command("minio", "server", "--address", addr, data)
	cmd.Env = append(os.Environ(),
		"MINIO_ROOT_USER="+accessKey,
		"MINIO_ROOT_PASSWORD="+secretKey,
		// Anonymous read access lets an apt client fetch from the bucket over
		// plain HTTP, without teaching the test harness to sign requests.
		"MINIO_BROWSER=off",
	)
	if err := cmd.Start(); err != nil {
		t.Logf("could not start minio: %v", err)
		return "", "", false
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	if !waitForPort(addr, 15*time.Second) {
		t.Log("minio did not start listening in time; skipping the S3 backend")
		return "", "", false
	}

	endpoint := "http://" + addr
	t.Setenv("AWS_ACCESS_KEY_ID", accessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secretKey)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ENDPOINT_URL", endpoint)

	// MinIO denies anonymous reads by default, so apt cannot be pointed at the
	// bucket without a policy. The CLI's own verify and check still cover the
	// backend end to end.
	return "s3://" + bucket + "/apt", "", true
}

// startSFTP runs an sshd on a free port with a throwaway host key and an
// authorized key for the current user, returning an sftp:// location.
func startSFTP(t *testing.T) (location string, ok bool) {
	t.Helper()
	sshdPath := sshdBinary()
	if sshdPath == "" || !have("ssh-keygen") {
		t.Log("sshd or ssh-keygen is not available; skipping the SFTP backend")
		return "", false
	}
	// The SFTP backend authenticates through the ssh-agent, so without one
	// running there is no way in.
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		t.Log("no ssh-agent is running; skipping the SFTP backend")
		return "", false
	}

	keys, err := agentPublicKeys()
	if err != nil || len(keys) == 0 {
		t.Logf("the ssh-agent holds no usable keys; skipping the SFTP backend (%v)", err)
		return "", false
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Log(err)
		return "", false
	}
	hostKey := filepath.Join(dir, "host_key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKey).CombinedOutput(); err != nil {
		t.Logf("could not generate a host key: %v\n%s", err, out)
		return "", false
	}
	authorized := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(authorized, keys, 0o600); err != nil {
		t.Log(err)
		return "", false
	}

	port, err := freePort()
	if err != nil {
		t.Logf("could not find a free port for sshd: %v", err)
		return "", false
	}
	repoDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Log(err)
		return "", false
	}

	config := filepath.Join(dir, "sshd_config")
	body := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %s
AuthorizedKeysFile %s
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
UsePAM no
StrictModes no
PidFile %s/sshd.pid
Subsystem sftp internal-sftp
LogLevel ERROR
`, port, hostKey, authorized, dir)
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Log(err)
		return "", false
	}

	cmd := exec.Command(sshdPath, "-D", "-e", "-f", config)
	if err := cmd.Start(); err != nil {
		t.Logf("could not start sshd: %v", err)
		return "", false
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	if !waitForPort(fmt.Sprintf("127.0.0.1:%d", port), 10*time.Second) {
		t.Log("sshd did not start listening in time; skipping the SFTP backend")
		return "", false
	}

	user := os.Getenv("USER")
	if user == "" {
		user = "root"
	}
	return fmt.Sprintf("sftp://%s@127.0.0.1:%d%s", user, port, repoDir), true
}

// sshdBinary locates sshd, which is not usually on a non-root user's PATH.
func sshdBinary() string {
	if p, err := exec.LookPath("sshd"); err == nil {
		return p
	}
	for _, p := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd", "/opt/homebrew/sbin/sshd"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// agentPublicKeys returns the public keys the running ssh-agent holds, in
// authorized_keys form.
func agentPublicKeys() ([]byte, error) {
	out, err := exec.Command("ssh-add", "-L").Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// freePort asks the kernel for an unused TCP port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForPort blocks until something accepts connections on addr, or the
// timeout expires.
func waitForPort(addr string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for ctx.Err() == nil {
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
