package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// Temporary AWS credentials (those from an assume-role, especially one guarded
// by MFA) live for an hour or more, but the SDK only caches them in memory: a
// second command is a second process, so it assumes the role again and prompts
// for a fresh MFA code. Since a token cannot be reused, that limits the user to
// one command per token. The provider here writes the session credentials to a
// file and reuses them until they expire, so only the first command of a
// session prompts.

// credCacheEnv overrides where session credentials are cached. Set it to a
// directory, or to "off" to disable caching (see credCacheDir).
// #nosec G101 -- the name of an environment variable, not a credential.
const credCacheEnv = "CREATEAPT_AWS_CACHE"

// credExpiryWindow is how long before their stated expiry cached credentials
// are treated as spent, so a long operation is not signed with credentials
// that lapse mid-run.
const credExpiryWindow = 5 * time.Minute

// cachedCreds is the on-disk form of one set of temporary credentials. The
// field names match the AWS CLI's own credential cache, so the files read the
// same way to anyone who has seen ~/.aws/cli/cache.
type cachedCreds struct {
	AccessKeyID     string    `json:"AccessKeyId"`
	SecretAccessKey string    `json:"SecretAccessKey"`
	SessionToken    string    `json:"SessionToken"`
	Expiration      time.Time `json:"Expiration"`
	AccountID       string    `json:"AccountId,omitempty"`
}

// cachingProvider retrieves credentials from the wrapped provider and keeps the
// result in a file, so later runs skip the round trip (and the MFA prompt that
// comes with it) until the credentials expire.
type cachingProvider struct {
	inner aws.CredentialsProvider
	path  string
	warn  io.Writer // where cache warnings go; os.Stderr in practice
	now   func() time.Time
}

// withCredentialCache wraps cfg's credential provider so that temporary
// credentials survive between runs of the program. It is a no-op unless the
// active profile assumes a role or names an MFA device: every other source is
// either free to re-resolve (instance metadata, credential_process, SSO, which
// caches its own token) or not temporary at all.
func withCredentialCache(ctx context.Context, cfg *aws.Config, warn io.Writer) {
	key, ok := credCacheKey(ctx)
	if !ok {
		return
	}
	dir, ok := credCacheDir()
	if !ok {
		return
	}
	p := &cachingProvider{
		inner: cfg.Credentials,
		path:  filepath.Join(dir, key+".json"),
		warn:  warn,
		now:   time.Now,
	}
	// The outer in-memory cache keeps every request in this process on one set
	// of credentials; the file is only consulted when that cache is cold.
	cfg.Credentials = aws.NewCredentialsCache(p, func(o *aws.CredentialsCacheOptions) {
		o.ExpiryWindow = credExpiryWindow
	})
}

// Retrieve implements aws.CredentialsProvider.
func (p *cachingProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	if creds, ok := p.read(); ok {
		return creds, nil
	}
	creds, err := p.inner.Retrieve(ctx)
	if err != nil {
		return creds, err
	}
	// Only session credentials are worth (or safe) storing: long-term keys have
	// no expiry and already live in the user's credentials file.
	if creds.CanExpire {
		if err := p.write(creds); err != nil && p.warn != nil {
			fmt.Fprintf(p.warn, "warning: could not cache AWS credentials in %s: %v\n", p.path, err)
		}
	}
	return creds, nil
}

// read returns the cached credentials if the file holds usable ones. Anything
// wrong with it — missing, unreadable, truncated, expired — just means a fresh
// retrieval, so no error is reported.
func (p *cachingProvider) read() (aws.Credentials, bool) {
	data, err := os.ReadFile(p.path)
	if err != nil {
		return aws.Credentials{}, false
	}
	var c cachedCreds
	if err := json.Unmarshal(data, &c); err != nil {
		return aws.Credentials{}, false
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.Expiration.IsZero() {
		return aws.Credentials{}, false
	}
	if !c.Expiration.After(p.now().Add(credExpiryWindow)) {
		return aws.Credentials{}, false
	}
	return aws.Credentials{
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.SecretAccessKey,
		SessionToken:    c.SessionToken,
		AccountID:       c.AccountID,
		Source:          "createapt-go credential cache",
		CanExpire:       true,
		Expires:         c.Expiration,
	}, true
}

// write stores creds for the next run. The file holds a secret, so it is
// created 0600 in a 0700 directory and swapped into place by rename, which
// keeps a concurrent reader from seeing a half-written file.
func (p *cachingProvider) write(creds aws.Credentials) error {
	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// #nosec G117 -- caching the session token is the point of this file;
	// it is written 0600 into a 0700 directory, as described above.
	data, err := json.Marshal(cachedCreds{
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
		Expiration:      creds.Expires,
		AccountID:       creds.AccountID,
	})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cred-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p.path)
}

// credCacheDir reports the directory holding cached credentials, and whether
// caching is on at all. It defaults to the user cache directory and can be
// pointed elsewhere, or turned off, with CREATEAPT_AWS_CACHE.
func credCacheDir() (string, bool) {
	if v := os.Getenv(credCacheEnv); v != "" {
		switch strings.ToLower(v) {
		case "off", "no", "none", "0", "false":
			return "", false
		}
		return v, true
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(base, "createapt-go", "aws"), true
}

// credCacheKey names the cache file for the credentials the active profile
// resolves to, and reports whether those credentials are worth caching. The key
// covers everything that decides which credentials come back, so a different
// profile, role, MFA device or credentials file never reuses this file's
// contents. It is hashed both to keep the name filesystem-safe and to keep role
// ARNs out of a directory listing.
func credCacheKey(ctx context.Context) (string, bool) {
	profile := os.Getenv("AWS_PROFILE")
	if profile == "" {
		profile = os.Getenv("AWS_DEFAULT_PROFILE")
	}
	if profile == "" {
		profile = "default"
	}
	configFile := os.Getenv("AWS_CONFIG_FILE")
	credsFile := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	// LoadSharedConfigProfile defaults to ~/.aws/{config,credentials} and, unlike
	// LoadDefaultConfig, does not itself honour the file location variables.
	sc, err := awsconfig.LoadSharedConfigProfile(ctx, profile, func(o *awsconfig.LoadSharedConfigOptions) {
		if configFile != "" {
			o.ConfigFiles = []string{configFile}
		}
		if credsFile != "" {
			o.CredentialsFiles = []string{credsFile}
		}
	})
	if err != nil {
		return "", false // no such profile, or no config file: nothing to cache
	}
	if sc.RoleARN == "" && sc.MFASerial == "" {
		return "", false
	}
	parts := []string{
		profile,
		sc.RoleARN,
		sc.MFASerial,
		sc.RoleSessionName,
		sc.ExternalID,
		sc.SourceProfileName,
		sc.CredentialSource,
		// The base identity the role is assumed from, when it comes from the
		// environment rather than a source profile.
		os.Getenv("AWS_ACCESS_KEY_ID"),
		configFile,
		credsFile,
	}
	if sc.RoleDurationSeconds != nil {
		parts = append(parts, sc.RoleDurationSeconds.String())
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:]), true
}
