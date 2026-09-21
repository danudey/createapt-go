package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// sftpBackend operates on a repository on a remote host over SSH/SFTP. File
// transfers use SFTP; checksums are computed remotely via `sha256sum` so an
// already-present package can be validated without downloading it.
type sftpBackend struct {
	client *sftp.Client
	ssh    *ssh.Client
	root   string // absolute remote path to the repo root
	label  string
}

func newSFTP(ctx context.Context, location string) (*sftpBackend, error) {
	u, err := url.Parse(location)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "22"
	}
	user := u.User.Username()
	if user == "" {
		user = os.Getenv("USER")
	}

	hkcb, err := hostKeyCallback()
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            sshAuthMethods(ctx, u),
		HostKeyCallback: hkcb,
	}
	addr := net.JoinHostPort(host, port)
	sshClient, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("sftp: dial %s: %w", addr, err)
	}
	sc, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return nil, fmt.Errorf("sftp: open session: %w", err)
	}
	return &sftpBackend{
		client: sc,
		ssh:    sshClient,
		root:   u.Path,
		label:  location,
	}, nil
}

// sshAuthMethods builds auth methods: the agent (if available) plus a password
// embedded in the URL (rare, but supported for automation).
func sshAuthMethods(ctx context.Context, u *url.URL) []ssh.AuthMethod {
	var methods []ssh.AuthMethod
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		var d net.Dialer
		// #nosec G704 -- a unix socket path from SSH_AUTH_SOCK, not a URL.
		if conn, err := d.DialContext(ctx, "unix", sock); err == nil {
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		}
	}
	if pw, ok := u.User.Password(); ok {
		methods = append(methods, ssh.Password(pw))
	}
	return methods
}

// InsecureIgnoreHostKey disables SSH host key verification for SFTP
// connections. It is set from the --insecure-ignore-host-key flag. With it
// unset (the default), a connection fails when known_hosts cannot be read or
// does not list the target host, rather than trusting whatever key is offered.
var InsecureIgnoreHostKey bool

// insecureHint is appended to host key failures so the message names the way out.
const insecureHint = "; add the host to known_hosts, or pass --insecure-ignore-host-key to connect without verification"

// hostKeyCallback verifies the remote host key against the user's known_hosts.
// It returns an error when known_hosts cannot be read, and the callback it
// returns rejects a host that file does not list. Verification is skipped
// entirely only when the user opted in via InsecureIgnoreHostKey.
func hostKeyCallback() (ssh.HostKeyCallback, error) {
	if InsecureIgnoreHostKey {
		// #nosec G106 -- explicitly requested with --insecure-ignore-host-key.
		return ssh.InsecureIgnoreHostKey(), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("sftp: locating known_hosts: %w%s", err, insecureHint)
	}
	kh := path.Join(home, ".ssh", "known_hosts")
	cb, err := knownhosts.New(kh)
	if err != nil {
		return nil, fmt.Errorf("sftp: reading %s: %w%s", kh, err, insecureHint)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(hostname, remote, key)
		// A KeyError with no candidate keys means the host is simply absent
		// from known_hosts; a populated Want means the key has changed.
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) && len(ke.Want) == 0 {
			return fmt.Errorf("sftp: host %s is not listed in %s%s", hostname, kh, insecureHint)
		}
		return err
	}, nil
}

func (s *sftpBackend) remote(relpath string) string {
	return path.Join(s.root, strings.TrimLeft(relpath, "/"))
}

func (s *sftpBackend) Get(_ context.Context, relpath string) (io.ReadCloser, error) {
	f, err := s.client.Open(s.remote(relpath))
	if os.IsNotExist(err) {
		return nil, ErrNotExist
	}
	return f, err
}

func (s *sftpBackend) Stat(_ context.Context, relpath string) (*FileInfo, error) {
	fi, err := s.client.Stat(s.remote(relpath))
	if os.IsNotExist(err) {
		return nil, ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	return &FileInfo{Size: fi.Size()}, nil
}

func (s *sftpBackend) Put(_ context.Context, relpath string, r io.Reader, _ int64) error {
	dst := s.remote(relpath)
	if err := s.client.MkdirAll(path.Dir(dst)); err != nil {
		return err
	}
	// Upload to a temp path then rename, so readers never see a partial file.
	tmp := dst + ".crtmp"
	f, err := s.client.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = s.client.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = s.client.Remove(tmp)
		return err
	}
	// sftp rename does not overwrite on all servers; use PosixRename when available.
	if err := s.client.PosixRename(tmp, dst); err != nil {
		// Fall back to remove+rename.
		_ = s.client.Remove(dst)
		if err2 := s.client.Rename(tmp, dst); err2 != nil {
			_ = s.client.Remove(tmp)
			return err2
		}
	}
	return nil
}

func (s *sftpBackend) Delete(_ context.Context, relpath string) error {
	err := s.client.Remove(s.remote(relpath))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *sftpBackend) String() string { return s.label }

// Copy duplicates src to dst on the remote host by running cp over an SSH
// session, so the package contents never cross the network (mirroring how Hash
// uses a remote sha256sum).
func (s *sftpBackend) Copy(_ context.Context, src, dst string) error {
	from := s.remote(src)
	to := s.remote(dst)
	if _, err := s.client.Stat(from); err != nil {
		if os.IsNotExist(err) {
			return ErrNotExist
		}
		return err
	}
	if err := s.client.MkdirAll(path.Dir(to)); err != nil {
		return err
	}
	sess, err := s.ssh.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	cmd := "cp -f -- " + shellQuote(from) + " " + shellQuote(to)
	if err := sess.Run(cmd); err != nil {
		return fmt.Errorf("sftp: remote copy %s -> %s: %w: %s", from, to, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (s *sftpBackend) Close() error {
	err := s.client.Close()
	if e := s.ssh.Close(); err == nil {
		err = e
	}
	return err
}

// List implements Lister by walking the remote tree under prefix and returning
// each regular file as a repo-relative, forward-slash path with its size. A
// missing prefix directory yields an empty list.
func (s *sftpBackend) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	root := s.remote(prefix)
	if _, err := s.client.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	base := strings.TrimRight(s.root, "/") + "/"
	var out []ObjectInfo
	w := s.client.Walk(root)
	for w.Step() {
		if err := w.Err(); err != nil {
			return nil, err
		}
		fi := w.Stat()
		if fi.IsDir() {
			continue
		}
		rel := strings.TrimPrefix(w.Path(), base)
		out = append(out, ObjectInfo{Path: rel, Size: fi.Size()})
	}
	return out, nil
}

// HashesContent reports that Hash reads the remote file itself (sha256sum runs
// over its bytes on the far side).
func (s *sftpBackend) HashesContent() bool { return true }

// Hash implements RemoteHasher by running sha256sum on the remote host, so the
// package contents never cross the network for validation.
func (s *sftpBackend) Hash(_ context.Context, relpath, algo string) (string, bool, error) {
	if algo != AlgoSHA256 {
		return "", false, nil
	}
	// Confirm the file exists first so a missing file is reported precisely.
	if _, err := s.client.Stat(s.remote(relpath)); err != nil {
		if os.IsNotExist(err) {
			return "", false, ErrNotExist
		}
		return "", false, err
	}
	sess, err := s.ssh.NewSession()
	if err != nil {
		return "", false, err
	}
	defer sess.Close()
	var out bytes.Buffer
	sess.Stdout = &out
	cmd := "sha256sum " + shellQuote(s.remote(relpath))
	if err := sess.Run(cmd); err != nil {
		// Remote host may lack sha256sum; signal "unknown" rather than fail.
		return "", false, nil
	}
	fields := strings.Fields(out.String())
	if len(fields) == 0 {
		return "", false, nil
	}
	return fields[0], true, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
