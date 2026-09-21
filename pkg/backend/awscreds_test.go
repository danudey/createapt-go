package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// countingProvider stands in for the assume-role provider: each Retrieve is one
// STS call, which in the MFA case is one prompt for a token the user cannot
// reuse.
type countingProvider struct {
	calls int
	creds aws.Credentials
	err   error
}

func (p *countingProvider) Retrieve(context.Context) (aws.Credentials, error) {
	p.calls++
	return p.creds, p.err
}

func sessionCreds(expires time.Time) aws.Credentials {
	return aws.Credentials{
		AccessKeyID:     "ASIAEXAMPLE",
		SecretAccessKey: "secret",
		SessionToken:    "token",
		CanExpire:       true,
		Expires:         expires,
	}
}

// newTestProvider builds a caching provider over inner, storing in dir.
func newTestProvider(dir string, inner aws.CredentialsProvider, warn *bytes.Buffer) *cachingProvider {
	return &cachingProvider{
		inner: inner,
		path:  filepath.Join(dir, "creds.json"),
		warn:  warn,
		now:   time.Now,
	}
}

func TestCredentialCacheReusedAcrossProviders(t *testing.T) {
	dir := t.TempDir()
	inner := &countingProvider{creds: sessionCreds(time.Now().Add(time.Hour))}
	var warn bytes.Buffer

	got, err := newTestProvider(dir, inner, &warn).Retrieve(context.Background())
	if err != nil {
		t.Fatalf("first Retrieve: %v", err)
	}
	if got.AccessKeyID != "ASIAEXAMPLE" {
		t.Errorf("AccessKeyID = %q, want ASIAEXAMPLE", got.AccessKeyID)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.calls)
	}

	// A second run of the program: a fresh provider over the same file must not
	// go back to STS.
	got, err = newTestProvider(dir, inner, &warn).Retrieve(context.Background())
	if err != nil {
		t.Fatalf("second Retrieve: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("inner calls = %d, want the cached credentials to be reused", inner.calls)
	}
	if got.SessionToken != "token" || got.SecretAccessKey != "secret" {
		t.Errorf("cached credentials = %+v, want the stored session", got)
	}
	if !got.CanExpire || got.Expires.IsZero() {
		t.Error("cached credentials lost their expiry")
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warning: %q", warn.String())
	}

	fi, err := os.Stat(filepath.Join(dir, "creds.json"))
	if err != nil {
		t.Fatalf("stat cache file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("cache file mode = %v, want 0600: it holds a secret", perm)
	}
}

func TestCredentialCacheRefreshesNearExpiry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expires time.Duration
	}{
		{"expired", -time.Minute},
		{"inside the expiry window", credExpiryWindow / 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			inner := &countingProvider{creds: sessionCreds(time.Now().Add(tc.expires))}
			p := newTestProvider(dir, inner, nil)
			if _, err := p.Retrieve(context.Background()); err != nil {
				t.Fatalf("first Retrieve: %v", err)
			}
			// The stored credentials are past their useful life, so the next run
			// must retrieve again rather than sign with them.
			inner.creds = sessionCreds(time.Now().Add(time.Hour))
			got, err := newTestProvider(dir, inner, nil).Retrieve(context.Background())
			if err != nil {
				t.Fatalf("second Retrieve: %v", err)
			}
			if inner.calls != 2 {
				t.Errorf("inner calls = %d, want 2 (cached credentials too close to expiry)", inner.calls)
			}
			if !got.Expires.After(time.Now().Add(credExpiryWindow)) {
				t.Errorf("returned credentials expire at %v, want the fresh ones", got.Expires)
			}
		})
	}
}

func TestCredentialCacheSkipsLongTermKeys(t *testing.T) {
	dir := t.TempDir()
	inner := &countingProvider{creds: aws.Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret"}}
	if _, err := newTestProvider(dir, inner, nil).Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "creds.json")); !os.IsNotExist(err) {
		t.Errorf("stat cache file = %v, want no file: long-term keys must not be copied out of the credentials file", err)
	}
}

func TestCredentialCacheIgnoresUnusableFile(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"corrupt", "{not json"},
		{"empty fields", `{"AccessKeyId":"","SecretAccessKey":"","Expiration":"2999-01-01T00:00:00Z"}`},
		{"no expiry", `{"AccessKeyId":"ASIA","SecretAccessKey":"s"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "creds.json"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			inner := &countingProvider{creds: sessionCreds(time.Now().Add(time.Hour))}
			if _, err := newTestProvider(dir, inner, nil).Retrieve(context.Background()); err != nil {
				t.Fatalf("Retrieve: %v", err)
			}
			if inner.calls != 1 {
				t.Errorf("inner calls = %d, want 1 (unusable cache file)", inner.calls)
			}
		})
	}
}

func TestCredentialCacheWriteFailureIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	// A file where the cache directory should be: writing cannot work.
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	inner := &countingProvider{creds: sessionCreds(time.Now().Add(time.Hour))}
	var warn bytes.Buffer
	p := &cachingProvider{inner: inner, path: filepath.Join(blocked, "creds.json"), warn: &warn, now: time.Now}
	got, err := p.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve = %v, want the credentials despite an unwritable cache", err)
	}
	if got.AccessKeyID != "ASIAEXAMPLE" {
		t.Errorf("AccessKeyID = %q, want the retrieved credentials", got.AccessKeyID)
	}
	if warn.Len() == 0 {
		t.Error("no warning about the unwritable cache")
	}
}

func TestCachedCredsFormat(t *testing.T) {
	dir := t.TempDir()
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	inner := &countingProvider{creds: sessionCreds(expires)}
	p := newTestProvider(dir, inner, nil)
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	data, err := os.ReadFile(p.path)
	if err != nil {
		t.Fatal(err)
	}
	var c cachedCreds
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("cache file is not JSON: %v", err)
	}
	if c.AccessKeyID != "ASIAEXAMPLE" || c.SecretAccessKey != "secret" || c.SessionToken != "token" {
		t.Errorf("cached credentials = %+v, want the retrieved session", c)
	}
	if !c.Expiration.Equal(expires) {
		t.Errorf("Expiration = %v, want %v", c.Expiration, expires)
	}
}

func TestCredCacheDir(t *testing.T) {
	t.Setenv(credCacheEnv, "/somewhere/else")
	if dir, ok := credCacheDir(); !ok || dir != "/somewhere/else" {
		t.Errorf("credCacheDir() = %q, %v, want the configured directory", dir, ok)
	}
	for _, off := range []string{"off", "OFF", "none", "no", "0", "false"} {
		t.Setenv(credCacheEnv, off)
		if dir, ok := credCacheDir(); ok {
			t.Errorf("credCacheDir() with %s=%q = %q, %v, want caching disabled", credCacheEnv, off, dir, ok)
		}
	}
	t.Setenv(credCacheEnv, "")
	dir, ok := credCacheDir()
	if !ok {
		t.Fatal("credCacheDir() disabled by default, want a default directory")
	}
	if filepath.Base(dir) != "aws" || filepath.Base(filepath.Dir(dir)) != "createapt-go" {
		t.Errorf("default cache dir = %q, want it under createapt-go/aws", dir)
	}
}

// writeAWSConfig points the AWS config-file variables at a config holding body
// and returns nothing: the profile is read through the environment, as it is in
// a real run.
func writeAWSConfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", path)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(filepath.Dir(path), "credentials"))
}

func TestCredCacheKey(t *testing.T) {
	const mfaProfile = `[profile base]
aws_access_key_id = AKIABASE
aws_secret_access_key = basesecret

[profile mfa]
role_arn = arn:aws:iam::123456789012:role/Publisher
source_profile = base
mfa_serial = arn:aws:iam::123456789012:mfa/dan

[profile plain]
region = eu-west-1

[profile other]
role_arn = arn:aws:iam::123456789012:role/Publisher
source_profile = base
mfa_serial = arn:aws:iam::123456789012:mfa/someone-else
`
	ctx := context.Background()

	writeAWSConfig(t, mfaProfile)
	t.Setenv("AWS_PROFILE", "mfa")
	key, ok := credCacheKey(ctx)
	if !ok {
		t.Fatal("credCacheKey() = false for an MFA assume-role profile, want it cached")
	}
	if len(key) != 64 {
		t.Errorf("key = %q, want a hex sha256", key)
	}
	if again, _ := credCacheKey(ctx); again != key {
		t.Errorf("key changed between calls: %q then %q", key, again)
	}

	// A different MFA device is a different identity: different file.
	t.Setenv("AWS_PROFILE", "other")
	if other, ok := credCacheKey(ctx); !ok || other == key {
		t.Errorf("key for a different profile = %q, %v, want a distinct key", other, ok)
	}

	// Nothing temporary to cache.
	t.Setenv("AWS_PROFILE", "plain")
	if _, ok := credCacheKey(ctx); ok {
		t.Error("credCacheKey() = true for a profile with no role or MFA device")
	}
	t.Setenv("AWS_PROFILE", "missing")
	if _, ok := credCacheKey(ctx); ok {
		t.Error("credCacheKey() = true for an unknown profile")
	}
}

func TestWithCredentialCacheLeavesPlainProfilesAlone(t *testing.T) {
	writeAWSConfig(t, "[profile plain]\nregion = eu-west-1\n")
	t.Setenv("AWS_PROFILE", "plain")
	inner := &countingProvider{creds: sessionCreds(time.Now().Add(time.Hour))}
	cfg := aws.Config{Credentials: inner}
	withCredentialCache(context.Background(), &cfg, nil)
	if cfg.Credentials != aws.CredentialsProvider(inner) {
		t.Error("withCredentialCache wrapped a profile with nothing worth caching")
	}
}

func TestWithCredentialCacheWrapsMFAProfile(t *testing.T) {
	writeAWSConfig(t, `[profile base]
aws_access_key_id = AKIABASE
aws_secret_access_key = basesecret

[profile mfa]
role_arn = arn:aws:iam::123456789012:role/Publisher
source_profile = base
mfa_serial = arn:aws:iam::123456789012:mfa/dan
`)
	t.Setenv("AWS_PROFILE", "mfa")
	dir := t.TempDir()
	t.Setenv(credCacheEnv, dir)

	inner := &countingProvider{creds: sessionCreds(time.Now().Add(time.Hour))}
	cfg := aws.Config{Credentials: inner}
	withCredentialCache(context.Background(), &cfg, nil)
	if _, err := cfg.Credentials.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("cache directory holds %d files, want 1", len(entries))
	}

	// Same config in a later run: the credentials come back from the file.
	cfg2 := aws.Config{Credentials: inner}
	withCredentialCache(context.Background(), &cfg2, nil)
	if _, err := cfg2.Credentials.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("inner calls = %d, want 1: the second run must reuse the cached credentials", inner.calls)
	}
}
