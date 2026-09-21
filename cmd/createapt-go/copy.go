package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/backend"
	"github.com/danudey/createapt-go/pkg/repo"
	"github.com/danudey/createapt-go/pkg/repoconfig"
	"github.com/danudey/createapt-go/pkg/sign"
)

// copyFlags holds the copy-specific options.
type copyFlags struct {
	// destination state
	overwrite bool
	resume    bool
	update    bool

	// transforms
	rebuildMetadata bool
	pruneOlder      bool
	repoName        string
	repoURL         string
	dstSuite        string
	dstComponent    string

	// selection
	latestOnly   bool
	include      []string
	exclude      []string
	arches       []string
	kinds        string
	excludeKinds string

	assumeYes bool
}

func copyCmd() *cobra.Command {
	var cf copyFlags
	cmd := &cobra.Command{
		Use:   "copy <source-repo> <destination-repo>",
		Short: "Copy a repository to another location",
		Long: `Read a suite from one location and write it to another. Source and destination
may each be any supported backend, so this mirrors between local disk, SSH/SFTP,
S3, GCS, and a read-only HTTP source.

By default the copy is exact: every file is transferred byte for byte, so the
destination is a replica of the source and the Release signature it carries
stays valid. Every file whose checksum the metadata records is verified as it
passes through, and a file already present at the destination with matching
content is not transferred again — an interrupted copy resumes cheaply with
--continue.

Any option that selects a subset of the packages, moves them, renames the suite
or component, or rebuilds the indexes switches the copy to regenerating the
destination's metadata. That invalidates the source's Release signature, so when
the source is signed such an option requires re-signing the copy with a key of
your own (--gpg-key or --gpg-key-id).

Writing into a suite that already exists needs one of:
  --overwrite  replace it; files the source does not have are deleted
  --continue   resume an interrupted copy of this same source repository
  --update     merge the source's packages into it (an incremental copy)

Examples:
  # Mirror a public repository onto S3, exactly as it stands.
  createapt-go copy https://deb.example.com/apt s3://my-bucket/apt --suite bookworm

  # Resume after an interruption.
  createapt-go copy https://deb.example.com/apt s3://my-bucket/apt --suite bookworm --continue

  # Take only the newest amd64 build of each package, no debug or source
  # packages, and sign the rebuilt indexes with our own key.
  createapt-go copy /srv/upstream /srv/mirror \
      --latest-only --arch amd64 --exclude-kinds debug,source \
      --gpg-key-id releases@example.com

  # Pull this week's new packages into an existing mirror.
  createapt-go copy /srv/upstream /srv/mirror --update`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCopy(cmd, args[0], args[1], &cf)
		},
	}

	f := cmd.Flags()
	f.BoolVar(&cf.overwrite, "overwrite", false, "replace an existing destination suite, deleting files the source does not have")
	f.BoolVar(&cf.resume, "continue", false, "resume an interrupted copy; fails if the destination is a different repository")
	f.BoolVar(&cf.update, "update", false, "add the source's packages to an existing destination suite")

	f.BoolVar(&cf.rebuildMetadata, "rebuild-metadata", false, "regenerate each package's index stanza from the copied .deb instead of carrying the source's over")
	f.BoolVar(&cf.pruneOlder, "prune-older", false, "after copying, drop superseded versions from the destination (--latest-only avoids transferring them in the first place)")
	f.BoolVar(&gf.pruneBreakDeps, "prune-break-deps", false, "when pruning, drop a version even if another package depends on it (default: keep it and warn)")
	f.StringVar(&cf.repoName, "repo-name", "", "human-readable repository name to record in the destination's config file")
	f.StringVar(&cf.repoURL, "repo-url", "", "public base URL of the copy, recorded in the destination's config file")
	f.StringVar(&cf.dstSuite, "to-suite", "", "publish into this suite at the destination instead of the source's")
	f.StringVar(&cf.dstComponent, "to-component", "", "index the copied packages in this component instead of the source's")

	f.BoolVar(&cf.latestOnly, "latest-only", false, "copy only the newest version of each package name+architecture")
	f.StringSliceVar(&cf.include, "include", nil, "only copy packages matching these glob patterns (name, name_version, or name_version_arch; repeatable)")
	f.StringSliceVar(&cf.exclude, "exclude", nil, "do not copy packages matching these glob patterns (repeatable)")
	f.StringSliceVar(&cf.arches, "arch", nil, `only copy these architectures ("all" packages are always kept; name "source" to keep source packages); fails if an arch is absent from the source`)
	f.StringVar(&cf.kinds, "kinds", "", "only copy these package kinds: binary, source, udeb, debug")
	f.StringVar(&cf.excludeKinds, "exclude-kinds", "", "do not copy these package kinds")

	f.BoolVar(&gf.removeUnreferencedPackages, "remove-unreferenced-packages", false, "delete pool files at the destination the copied indexes do not reference")
	f.BoolVar(&gf.removeStaleMetadata, "remove-stale-metadata", false, "delete index files at the destination that are no longer in use")
	f.BoolVarP(&cf.assumeYes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// runCopy drives the whole operation: open and verify the source, decide what
// the destination may look like, then either replicate it byte for byte or
// rebuild it from the selected packages.
func runCopy(cmd *cobra.Command, srcLoc, dstLoc string, cf *copyFlags) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	if err := cf.validate(); err != nil {
		return err
	}

	// --- source ---------------------------------------------------------
	srcBE, err := backend.Open(ctx(cmd), srcLoc)
	if err != nil {
		return err
	}
	srcCfg, err := repoconfig.Load(ctx(cmd), srcBE)
	if err != nil {
		closeBackend(srcBE)
		return err
	}
	srcSuite := gf.suite
	if srcSuite == "" && srcCfg != nil {
		srcSuite = srcCfg.Suite
	}
	src, err := repo.OpenWith(ctx(cmd), srcBE, repo.Options{Suite: srcSuite})
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	defer src.Close()

	keyrings, releaseKeys := verificationKeyrings(cmd, srcCfg)
	defer releaseKeys()

	sourceSigned, err := verifySourceRelease(cmd, src, keyrings)
	if err != nil {
		return err
	}

	// --- selection ------------------------------------------------------
	filter, err := cf.filter()
	if err != nil {
		return err
	}
	all := src.Index().Entries()
	selected, err := filter.Apply(all)
	if err != nil {
		return err
	}
	// An empty source suite is a legitimate thing to copy; an empty selection
	// out of a non-empty one means the filters matched nothing.
	if len(selected) == 0 && len(all) > 0 {
		return fmt.Errorf("no packages selected from %s (%d in the source); check --include/--exclude/--arch/--kinds", srcLoc, len(all))
	}

	// --- destination state ----------------------------------------------
	dstSuite := cf.dstSuite
	if dstSuite == "" {
		dstSuite = src.Suite()
	}
	dstBE, err := backend.Open(ctx(cmd), dstLoc)
	if err != nil {
		return err
	}
	dstExists, err := suiteExists(cmd, dstBE, dstSuite)
	if err != nil {
		closeBackend(dstBE)
		return err
	}
	if dstExists && !cf.overwrite && !cf.resume && !cf.update {
		closeBackend(dstBE)
		return fmt.Errorf("the %s suite already exists at %s; pass --overwrite to replace it, --continue to resume an interrupted copy, or --update to add to it", dstSuite, dstLoc)
	}
	if !dstExists && cf.update {
		closeBackend(dstBE)
		return fmt.Errorf("--update needs an existing %s suite at %s; there is none", dstSuite, dstLoc)
	}

	// --- mode -----------------------------------------------------------
	relocate := cmd.Flags().Changed("pool-layout")
	renames := dstSuite != src.Suite() || cf.dstComponent != ""
	transform := cf.update || cf.rebuildMetadata || cf.pruneOlder ||
		!filter.Empty() || relocate || renames

	if cf.resume {
		if err := checkResumable(cmd, srcLoc, dstBE, dstSuite, dstExists, src); err != nil {
			closeBackend(dstBE)
			return err
		}
	}

	if !transform {
		return copyExact(cmd, src, srcLoc, srcCfg, dstBE, dstLoc, dstExists, sourceSigned, cf)
	}

	// Set the destination's suite and component before the repo is opened, so
	// the recorded-config layer and the index both see them.
	gf.suite = dstSuite
	if cf.dstComponent != "" {
		gf.component = cf.dstComponent
	}

	return copyRebuilding(cmd, copyRun{
		src: src, srcLoc: srcLoc, srcCfg: srcCfg, sourceSigned: sourceSigned,
		dstBE: dstBE, dstLoc: dstLoc, dstExists: dstExists, dstSuite: dstSuite,
		selected: selected, cf: cf, relocate: relocate,
		out: out, errOut: errOut,
	})
}

// validate rejects incoherent flag combinations before anything is opened.
func (cf *copyFlags) validate() error {
	set := 0
	for _, b := range []bool{cf.overwrite, cf.resume, cf.update} {
		if b {
			set++
		}
	}
	if set > 1 {
		return fmt.Errorf("--overwrite, --continue and --update are mutually exclusive")
	}
	return nil
}

// filter builds the package selection from the copy flags.
func (cf *copyFlags) filter() (aptdata.Filter, error) {
	kinds, err := aptdata.ParseKinds(cf.kinds)
	if err != nil {
		return aptdata.Filter{}, err
	}
	excludeKinds, err := aptdata.ParseKinds(cf.excludeKinds)
	if err != nil {
		return aptdata.Filter{}, err
	}
	return aptdata.Filter{
		Include:      cf.include,
		Exclude:      cf.exclude,
		Arches:       cf.arches,
		Kinds:        kinds,
		ExcludeKinds: excludeKinds,
		LatestOnly:   cf.latestOnly,
	}, nil
}

// suiteExists reports whether a suite has already been published at be.
func suiteExists(cmd *cobra.Command, be backend.Backend, suite string) (bool, error) {
	for _, p := range []string{aptdata.ReleasePath(suite), aptdata.InReleasePath(suite)} {
		_, err := be.Stat(ctx(cmd), p)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, backend.ErrNotExist):
			continue
		default:
			return false, err
		}
	}
	return false, nil
}

// verificationKeyrings resolves the public keys the copy validates against: the
// ones the user supplied with --keyring, or failing that the source
// repository's own signing key, exported from the local GnuPG keyring by the
// fingerprint recorded in its config. A key that cannot be resolved is a
// warning, not an error — verification is then simply not possible.
func verificationKeyrings(cmd *cobra.Command, srcCfg *repoconfig.Config) ([]string, func()) {
	noop := func() {}
	if gf.skipVerify {
		return nil, noop
	}
	if len(gf.keyrings) > 0 {
		return gf.keyrings, noop
	}
	if srcCfg == nil || srcCfg.GPGKeyID == "" {
		return nil, noop
	}
	path, cleanup, err := sign.ExportPublicKeyToFile(srcCfg.GPGKeyID)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: the source repository is signed with %s but that key is not available locally (%v); signatures will not be verified. Pass --keyring to supply it.\n",
			srcCfg.GPGKeyID, err)
		return nil, noop
	}
	fmt.Fprintf(cmd.OutOrStdout(), "verifying against the source's recorded signing key %s\n", srcCfg.GPGKeyID)
	return []string{path}, cleanup
}

// verifySourceRelease checks the source suite's Release signature, in whichever
// of its two forms is present. It reports whether the source is signed at all,
// which decides later whether a transforming copy must be re-signed.
func verifySourceRelease(cmd *cobra.Command, src *repo.Repo, keyrings []string) (bool, error) {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	inRelease, detached, err := src.ReleaseSignatures(ctx(cmd))
	if err != nil {
		return false, fmt.Errorf("reading the source's Release signature: %w", err)
	}
	signed := inRelease != nil || detached != nil

	if gf.skipVerify {
		if signed {
			fmt.Fprintln(errOut, "warning: --skip-verify: the source's Release signature is not being checked")
		}
		return signed, nil
	}
	if len(keyrings) == 0 {
		if gf.verifySigs {
			return signed, fmt.Errorf("--verify-sigs needs a public key: pass --keyring, or copy a repository whose %s records its signing key", repoconfig.Path)
		}
		if signed {
			fmt.Fprintf(errOut, "warning: the source's Release is signed but no public key is available to check it; pass --keyring to verify the copy\n")
		}
		return signed, nil
	}
	if !signed {
		if gf.verifySigs {
			return false, fmt.Errorf("--verify-sigs: the source suite %s carries no Release signature", src.Suite())
		}
		fmt.Fprintf(errOut, "warning: the source's Release is not signed (no InRelease or Release.gpg)\n")
		return false, nil
	}

	// InRelease is preferred: it is one document, so what was verified is
	// exactly what was read.
	if inRelease != nil {
		if _, fpr, err := sign.VerifyClearsigned(inRelease, keyrings); err != nil {
			return true, fmt.Errorf("the source's InRelease signature does not verify: %w", err)
		} else {
			fmt.Fprintf(out, "verified: InRelease signed by %s\n", fpr)
			return true, nil
		}
	}

	raw, err := src.RawRelease(ctx(cmd))
	if err != nil {
		return true, fmt.Errorf("reading the source's Release: %w", err)
	}
	fpr, err := sign.VerifyDetached(raw, detached, keyrings)
	if err != nil {
		return true, fmt.Errorf("the source's Release.gpg signature does not verify: %w", err)
	}
	fmt.Fprintf(out, "verified: Release signed by %s\n", fpr)
	return true, nil
}

// checkResumable confirms that --continue is resuming a copy of this same
// source: the destination must record the same origin, or hold nothing the
// source does not have.
func checkResumable(cmd *cobra.Command, srcLoc string, dstBE backend.Backend, dstSuite string, dstExists bool, src *repo.Repo) error {
	dstCfg, err := repoconfig.Load(ctx(cmd), dstBE)
	if err != nil {
		return err
	}
	if dstCfg != nil && dstCfg.CopySource != "" && dstCfg.CopySource != srcLoc {
		return fmt.Errorf("--continue: %s was copied from %s, not from %s", dstBE, dstCfg.CopySource, srcLoc)
	}
	if !dstExists {
		return nil
	}
	dst, err := repo.OpenWith(ctx(cmd), dstBE, repo.Options{Suite: dstSuite})
	if err != nil {
		return fmt.Errorf("--continue: reading the destination: %w", err)
	}
	// The backend is shared with the caller, which owns closing it.
	if err := repo.SameRepository(src.Index(), dst.Index()); err != nil {
		return fmt.Errorf("--continue: %s is not a partial copy of %s: %w", dstBE, srcLoc, err)
	}
	return nil
}
