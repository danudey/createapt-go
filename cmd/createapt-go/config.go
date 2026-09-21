package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/danudey/createapt-go/pkg/backend"
	"github.com/danudey/createapt-go/pkg/repo"
	"github.com/danudey/createapt-go/pkg/repoconfig"
	"github.com/danudey/createapt-go/pkg/sign"
)

// openRepo opens the backend for location, loads any persisted repository
// config, applies the recorded settings as defaults for flags the user did not
// set explicitly, and opens the repository. It returns the repo along with the
// previously-persisted config (nil if none) so a later Save can preserve fields
// (name/baseurl) it does not manage.
func openRepo(cmd *cobra.Command, location string, create bool) (*repo.Repo, *repoconfig.Config, error) {
	be, err := backend.Open(ctx(cmd), location)
	if err != nil {
		return nil, nil, err
	}
	return openRepoBackend(cmd, be, create)
}

// openRepoBackend is openRepo for a backend the caller has already opened (and
// whose config it may already have inspected). It takes ownership of be: on
// success the returned Repo closes it, and on failure it is closed here.
func openRepoBackend(cmd *cobra.Command, be backend.Backend, create bool) (*repo.Repo, *repoconfig.Config, error) {
	prev, err := repoconfig.Load(ctx(cmd), be)
	if err != nil {
		closeBackend(be)
		return nil, nil, err
	}
	if err := applyConfigDefaults(cmd, prev); err != nil {
		closeBackend(be)
		return nil, nil, err
	}
	if err := validateSigningFlags(); err != nil {
		closeBackend(be)
		return nil, nil, err
	}

	opts, err := repoOptions(create)
	if err != nil {
		closeBackend(be)
		return nil, nil, err
	}
	r, err := repo.OpenWith(ctx(cmd), be, opts)
	if err != nil {
		return nil, nil, err
	}
	return r, prev, nil
}

// openRepoForRead opens a repository for a command that only inspects it
// (list, verify, check). It applies the recorded defaults exactly as a
// mutating command does — so `verify /srv/repo` checks the suite the
// repository was published into, not whatever the default suite happens to be
// — but never builds a signer, because nothing is going to be signed.
func openRepoForRead(cmd *cobra.Command, location string) (*repo.Repo, error) {
	be, err := backend.Open(ctx(cmd), location)
	if err != nil {
		return nil, err
	}
	prev, err := repoconfig.Load(ctx(cmd), be)
	if err != nil {
		closeBackend(be)
		return nil, err
	}
	if err := applyConfigDefaults(cmd, prev); err != nil {
		closeBackend(be)
		return nil, err
	}
	gf.signRelease = false

	opts, err := repoOptions(false)
	if err != nil {
		closeBackend(be)
		return nil, err
	}
	return repo.OpenWith(ctx(cmd), be, opts)
}

// applyConfigDefaults fills in global flags from the persisted config for any
// flag the user did not set on the command line. This makes the recorded
// settings act as defaults: a repository created for a given --target keeps
// using that profile's defaults, one created with a given suite and component
// keeps receiving packages there, and one signed once keeps being signed with
// the same key, without re-specifying the flags.
//
// Precedence, high to low: explicit CLI flags; an explicit CLI --target's
// profile; the recorded --target's profile; the individually recorded settings.
func applyConfigDefaults(cmd *cobra.Command, cfg *repoconfig.Config) error {
	if cfg == nil {
		return nil
	}
	fl := cmd.Flags()

	// Restore the recorded --target (unless the user gave one) and re-resolve
	// its profile so its compression/hash defaults apply. preRunE already
	// resolved a CLI-supplied target; this covers the config-supplied one,
	// which is not known until the config is loaded.
	if !fl.Changed("target") && cfg.Target != "" {
		gf.target = cfg.Target
		if err := applyProfile(cmd, nil); err != nil {
			return fmt.Errorf("recorded --target %q in %s: %w", cfg.Target, repoconfig.Path, err)
		}
	}

	// Keep operating on the suite and component the repository was last
	// published with, so an `add` that omits them updates the existing suite
	// rather than quietly creating a second one beside it.
	if !fl.Changed("suite") && cfg.Suite != "" {
		gf.suite = cfg.Suite
	}
	if !fl.Changed("component") && cfg.Component != "" {
		gf.component = cfg.Component
	}
	if !fl.Changed("pool-layout") {
		gf.poolLayout = cfg.PoolLayout
	}
	if !fl.Changed("by-hash") {
		gf.byHash = cfg.ByHash
	}

	for _, pair := range []struct {
		flag  string
		field *string
		value string
	}{
		{"origin", &gf.origin, cfg.Origin},
		{"label", &gf.label, cfg.Label},
		{"codename", &gf.codename, cfg.Codename},
		{"release-version", &gf.releaseVersion, cfg.Version},
	} {
		if !fl.Changed(pair.flag) && pair.value != "" {
			*pair.field = pair.value
		}
	}
	if gf.description == "" && cfg.Description != "" {
		gf.description = cfg.Description
	}

	if !fl.Changed("sign-release") && cfg.SignRelease {
		gf.signRelease = true
	}
	if !fl.Changed("gpg-key") && !fl.Changed("gpg-key-id") && cfg.GPGKeyID != "" {
		gf.gpgKeyID = cfg.GPGKeyID
	}
	return nil
}

// saveRepoConfig writes the repository config after a successful commit,
// recording the effective settings (with the key stored as a fingerprint) and
// preserving/overriding the name and base URL. It is a no-op in dry-run mode.
// Failures are reported but do not fail the command: the packages and indexes
// have already been published at this point.
func saveRepoConfig(cmd *cobra.Command, r *repo.Repo, prev *repoconfig.Config, name, baseURL string) {
	cfg := effectiveConfig(cmd, prev, name, baseURL)
	writeRepoConfig(cmd, r.Backend(), &cfg)
}

// effectiveConfig merges the previously-persisted config with the settings this
// invocation ended up using, returning what should be written back.
func effectiveConfig(cmd *cobra.Command, prev *repoconfig.Config, name, baseURL string) repoconfig.Config {
	cfg := repoconfig.Config{}
	if prev != nil {
		cfg = *prev
	}
	if name != "" {
		cfg.Name = name
	}
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}

	cfg.Target = gf.target
	cfg.Suite = effectiveSuite()
	cfg.Component = effectiveComponent()
	cfg.PoolLayout = gf.poolLayout
	cfg.ByHash = gf.byHash
	cfg.Origin = gf.origin
	cfg.Label = gf.label
	cfg.Codename = gf.codename
	cfg.Version = gf.releaseVersion
	cfg.Description = gf.description
	cfg.SignRelease = gf.signRelease
	if gf.signRelease {
		if fpr, err := sign.Fingerprint(gf.gpgKey, gf.gpgKeyID); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not resolve signing key fingerprint: %v\n", err)
		} else if fpr != "" {
			cfg.GPGKeyID = fpr
		}
	}
	return cfg
}

// baseURLOf picks the repository's public URL: the one just given, falling
// back to whatever a previous run recorded.
func baseURLOf(prev *repoconfig.Config, given string) string {
	if given != "" {
		return given
	}
	if prev != nil {
		return prev.BaseURL
	}
	return ""
}

func effectiveSuite() string {
	if gf.suite == "" {
		return repo.DefaultSuite
	}
	return gf.suite
}

func effectiveComponent() string {
	if gf.component == "" {
		return repo.DefaultComponent
	}
	return gf.component
}

// writeRepoConfig persists cfg to the repository root. It is a no-op in dry-run
// mode, and a failure is reported without failing the command: by this point
// the packages and indexes have already been published.
func writeRepoConfig(cmd *cobra.Command, be backend.Backend, cfg *repoconfig.Config) {
	out := cmd.OutOrStdout()
	if gf.dryRun {
		fmt.Fprintf(out, "[dry-run] config: would write %s\n", repoconfig.Path)
		return
	}
	if err := repoconfig.Save(ctx(cmd), be, cfg); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not write %s: %v\n", repoconfig.Path, err)
		return
	}
	fmt.Fprintf(out, "config: wrote %s\n", repoconfig.Path)
}

// closeBackend closes a backend that holds resources, ignoring errors. It is
// used on error paths before a Repo (which would otherwise own the close) is
// constructed.
func closeBackend(be backend.Backend) {
	if c, ok := be.(backend.Closer); ok {
		_ = c.Close()
	}
}
