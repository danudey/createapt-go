package main

import (
	"strings"
	"testing"

	"github.com/danudey/createapt-go/pkg/repoconfig"
	"github.com/spf13/cobra"
)

func TestResolveProfile(t *testing.T) {
	const (
		defaultCompression = "gzip,xz"
		defaultHashes      = "md5,sha256"
	)

	tests := []struct {
		name           string
		target         string
		compressionSet bool
		hashesSet      bool
		wantComp       string
		wantHashes     string
	}{
		{
			name:     "no target leaves the defaults alone",
			wantComp: defaultCompression, wantHashes: defaultHashes,
		},
		{
			name:   "a canonical target applies its profile",
			target: "legacy", wantComp: "gzip", wantHashes: "md5,sha1,sha256",
		},
		{
			name:   "a distribution alias resolves to its profile",
			target: "bookworm", wantComp: "gzip,xz,zstd", wantHashes: "sha256",
		},
		{
			name:   "an alias is matched case-insensitively",
			target: "JAMMY", wantComp: "gzip,xz,zstd", wantHashes: "sha256",
		},
		{
			name:   "an explicit compression wins over the profile",
			target: "legacy", compressionSet: true,
			wantComp: defaultCompression, wantHashes: "md5,sha1,sha256",
		},
		{
			name:   "an explicit hash set wins over the profile",
			target: "legacy", hashesSet: true,
			wantComp: "gzip", wantHashes: defaultHashes,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			comp, hashes, err := resolveProfile(tc.target,
				defaultCompression, defaultHashes, tc.compressionSet, tc.hashesSet)
			if err != nil {
				t.Fatal(err)
			}
			if comp != tc.wantComp {
				t.Errorf("compression = %q; want %q", comp, tc.wantComp)
			}
			if hashes != tc.wantHashes {
				t.Errorf("hashes = %q; want %q", hashes, tc.wantHashes)
			}
		})
	}
}

func TestResolveProfileRejectsAnUnknownTarget(t *testing.T) {
	_, _, err := resolveProfile("windows11", "gzip", "sha256", false, false)
	if err == nil {
		t.Fatal("an unknown --target was accepted")
	}
	// The message has to name what is valid, or the user has nowhere to go.
	if !strings.Contains(err.Error(), "legacy") || !strings.Contains(err.Error(), "debian12") {
		t.Errorf("the error does not say what is valid: %v", err)
	}
}

func TestEveryProfileAliasResolves(t *testing.T) {
	for alias, canonical := range profileAliases {
		if _, ok := profiles[canonical]; !ok {
			t.Errorf("alias %q points at %q, which is not a profile", alias, canonical)
		}
	}
}

func TestValidateSigningFlags(t *testing.T) {
	tests := []struct {
		name    string
		flags   globalFlags
		wantErr string
	}{
		{name: "nothing set is fine"},
		{
			name:    "a key with nothing to sign is a mistake",
			flags:   globalFlags{gpgKeyID: "releases@example.com"},
			wantErr: "--sign-release",
		},
		{
			name:  "a key with something to sign is fine",
			flags: globalFlags{gpgKeyID: "releases@example.com", signRelease: true},
		},
		{
			name:    "two key sources are ambiguous",
			flags:   globalFlags{gpgKey: "k.asc", gpgKeyID: "id", signRelease: true},
			wantErr: "only one of",
		},
		{
			name:    "verifying and skipping verification contradict",
			flags:   globalFlags{verifySigs: true, skipVerify: true},
			wantErr: "contradict",
		},
	}

	saved := gf
	t.Cleanup(func() { gf = saved })

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gf = tc.flags
			err := validateSigningFlags()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("expected an error mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %v does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestApplyConfigDefaultsOnlyFillsUnsetFlags is the precedence rule the config
// file depends on: what the operator typed always wins over what was recorded.
func TestApplyConfigDefaultsOnlyFillsUnsetFlags(t *testing.T) {
	saved := gf
	t.Cleanup(func() { gf = saved })

	cfg := &repoconfig.Config{
		Suite: "jammy", Component: "contrib", Target: "legacy",
		PoolLayout: true, ByHash: true,
		Origin: "Recorded", SignRelease: true, GPGKeyID: "FPR",
	}

	t.Run("an unset flag takes the recorded value", func(t *testing.T) {
		gf = globalFlags{compression: "gzip,xz", hashes: "md5,sha256"}
		cmd := testCommand()
		if err := applyConfigDefaults(cmd, cfg); err != nil {
			t.Fatal(err)
		}
		if gf.suite != "jammy" || gf.component != "contrib" {
			t.Errorf("suite/component = %q/%q; want the recorded jammy/contrib", gf.suite, gf.component)
		}
		if !gf.signRelease || gf.gpgKeyID != "FPR" {
			t.Errorf("the recorded signing settings were not applied: %+v", gf)
		}
		if gf.origin != "Recorded" {
			t.Errorf("origin = %q; want the recorded value", gf.origin)
		}
		// The recorded target's profile has to be re-resolved, not just stored.
		if gf.compression != "gzip" || gf.hashes != "md5,sha1,sha256" {
			t.Errorf("the recorded target's profile was not applied: compression=%q hashes=%q",
				gf.compression, gf.hashes)
		}
	})

	t.Run("an explicit flag wins", func(t *testing.T) {
		gf = globalFlags{suite: "trixie", compression: "gzip,xz", hashes: "md5,sha256"}
		cmd := testCommand()
		if err := cmd.Flags().Set("suite", "trixie"); err != nil {
			t.Fatal(err)
		}
		if err := applyConfigDefaults(cmd, cfg); err != nil {
			t.Fatal(err)
		}
		if gf.suite != "trixie" {
			t.Errorf("suite = %q; want the explicitly given trixie", gf.suite)
		}
		// The component was not given, so it still comes from the config.
		if gf.component != "contrib" {
			t.Errorf("component = %q; want the recorded contrib", gf.component)
		}
	})

	t.Run("a nil config changes nothing", func(t *testing.T) {
		gf = globalFlags{suite: "trixie"}
		if err := applyConfigDefaults(testCommand(), nil); err != nil {
			t.Fatal(err)
		}
		if gf.suite != "trixie" {
			t.Errorf("suite = %q; want it untouched", gf.suite)
		}
	})
}

// testCommand builds a command carrying the persistent flags applyConfigDefaults
// inspects, so Changed() reports what a real invocation would.
func testCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	f := cmd.Flags()
	for _, name := range []string{"target", "suite", "component", "origin", "label", "codename", "release-version"} {
		f.String(name, "", "")
	}
	for _, name := range []string{"pool-layout", "by-hash", "sign-release", "gpg-key", "gpg-key-id"} {
		f.Bool(name, false, "")
	}
	for _, name := range []string{"compression", "hashes"} {
		f.String(name, "", "")
	}
	return cmd
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KiB",
		1536: "1.5 KiB", 1048576: "1.0 MiB", 1073741824: "1.0 GiB",
	} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q; want %q", in, got, want)
		}
	}
}

func TestHostArchUsesDebianNames(t *testing.T) {
	// Whatever this machine is, the name must be one Debian uses — never a Go
	// GOARCH spelling that no repository would have an index for.
	got := hostArch()
	for _, bad := range []string{"386", "ppc64le"} {
		if got == bad {
			t.Errorf("hostArch() = %q, which is a Go name, not a Debian one", got)
		}
	}
	if got == "" {
		t.Error("hostArch() returned an empty architecture")
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList(" a , b ,, c "); strings.Join(got, "|") != "a|b|c" {
		t.Errorf("splitList trimmed or split wrongly: %v", got)
	}
	if got := splitList(""); len(got) != 0 {
		t.Errorf("splitList(\"\") = %v; want nothing", got)
	}
}
