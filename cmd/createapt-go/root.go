package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/backend"
	"github.com/danudey/createapt-go/pkg/repo"
	"github.com/danudey/createapt-go/pkg/sign"
)

// version is overridable at build time with -ldflags.
var version = "dev"

// globalFlags holds options shared across subcommands.
type globalFlags struct {
	target      string
	suite       string
	component   string
	arches      string
	hashes      string
	compression string
	poolLayout  bool
	byHash      bool
	validity    time.Duration
	dryRun      bool
	force       bool

	// Release file identity.
	origin         string
	label          string
	codename       string
	releaseVersion string
	description    string

	// pruneBreakDeps permits add/rebuild --prune-older to drop a version even
	// when another package still depends on that specific version.
	pruneBreakDeps bool

	// rebuild-only directory-scan cleanups.
	removeUnreferencedPackages bool
	removeStaleMetadata        bool

	// AWS/S3 credentials selection. These map onto the AWS_PROFILE / AWS_REGION
	// environment variables, which the AWS SDK's default config loader honours.
	awsProfile string
	awsRegion  string

	// insecureIgnoreHostKey disables SSH host key verification for sftp://
	// locations. Without it, an unreadable known_hosts or an unlisted host is
	// a connection failure.
	insecureIgnoreHostKey bool

	// signing / verification
	signRelease bool
	verifySigs  bool
	skipVerify  bool
	gpgKey      string
	gpgKeyID    string
	gpgPass     string
	keyrings    []string
}

// compatProfile is the set of defaults a --target selects.
//
// Unlike an rpm repository, where the client's version dictates which package
// signature it can read, an apt client's constraint is which index compressions
// and which Release hashes it understands. Those are the two axes a target
// moves.
type compatProfile struct {
	compression string
	hashes      string
}

// profiles maps a release target to its recommended defaults.
//
//   - legacy: apt before 1.1 (Debian 8, Ubuntu 14.04/16.04) reads gzip indexes
//     and wants an MD5Sum block; it does not read xz indexes reliably.
//   - modern: apt 1.1 through 2.2 (Debian 9-11, Ubuntu 18.04-20.04) reads xz,
//     which is markedly smaller than gzip for an index.
//   - zstd: apt 2.3+ (Debian 12+, Ubuntu 22.04+) also reads zstd, which
//     decompresses far faster for about the same size as xz.
var profiles = map[string]compatProfile{
	profileLegacy: {compression: "gzip", hashes: "md5,sha1,sha256"},
	profileModern: {compression: "gzip,xz", hashes: "md5,sha256"},
	profileZstd:   {compression: "gzip,xz,zstd", hashes: "sha256"},
}

// The canonical profile keys --target accepts.
const (
	profileLegacy = "legacy"
	profileModern = "modern"
	profileZstd   = "zstd"
)

// profileAliases maps release names to canonical profile keys, so a user can
// name the oldest distribution they intend to serve rather than reason about
// compression support.
var profileAliases = map[string]string{
	"debian8": profileLegacy, "jessie": profileLegacy,
	"debian9": profileModern, "stretch": profileModern,
	"debian10": profileModern, "buster": profileModern,
	"debian11": profileModern, "bullseye": profileModern,
	"debian12": profileZstd, "bookworm": profileZstd,
	"debian13": profileZstd, "trixie": profileZstd,

	"ubuntu14.04": profileLegacy, "trusty": profileLegacy,
	"ubuntu16.04": profileLegacy, "xenial": profileLegacy,
	"ubuntu18.04": profileModern, "bionic": profileModern,
	"ubuntu20.04": profileModern, "focal": profileModern,
	"ubuntu22.04": profileZstd, "jammy": profileZstd,
	"ubuntu24.04": profileZstd, "noble": profileZstd,
	"ubuntu26.04": profileZstd, "resolute": profileZstd,
}

var gf globalFlags

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "createapt-go",
		Short: "Lightweight apt (Debian/Ubuntu) repository manager",
		Long: `createapt-go creates and maintains apt repositories on local disk or remote
storage (SSH/SFTP, S3, GCS; HTTP(S) read-only).

It updates a repository in place, transferring as little data as possible:
only the (small) indexes are fetched and rewritten, packages already present
are validated remotely by checksum rather than re-uploaded, and existing
packages are never downloaded.`,
		SilenceUsage:      true,
		SilenceErrors:     true,
		Version:           version,
		PersistentPreRunE: preRunE,
	}

	pf := root.PersistentFlags()
	pf.StringVar(&gf.target, "target", "", "compatibility profile setting defaults: legacy, modern, zstd (aliases: debian12, jammy, noble, ...)")
	pf.StringVar(&gf.suite, "suite", "", "suite (dists/ subdirectory) to operate on (default "+repo.DefaultSuite+")")
	pf.StringVar(&gf.component, "component", "", "component (section) to place added packages in (default "+repo.DefaultComponent+")")
	pf.StringVar(&gf.arches, "architectures", "", "architectures the suite publishes an index for (default: those of the indexed packages, plus any the previous Release listed)")
	pf.StringVar(&gf.hashes, "hashes", "md5,sha256", "hashes written into the indexes and Release: md5, sha1, sha256 (sha256 is always included)")
	pf.StringVar(&gf.compression, "compression", "gzip,xz", "index compression forms: none, gzip, xz, zstd (the uncompressed form is always written)")
	pf.BoolVar(&gf.poolLayout, "pool-layout", true, "place packages in the conventional pool/<component>/<prefix>/<source>/ tree (false: flat under pool/<component>)")
	pf.BoolVar(&gf.byHash, "by-hash", true, "publish each index under its checksum too and advertise Acquire-By-Hash, so a publish is near-atomic")
	pf.DurationVar(&gf.validity, "validity", 0, "set the Release file's Valid-Until this long after its Date (0: never expires)")
	pf.StringVar(&gf.awsProfile, "profile", "", "AWS named profile for S3 access (sets AWS_PROFILE; MFA-protected assume-role profiles are prompted for on stdin)")
	pf.StringVar(&gf.awsRegion, "region", "", "AWS region for S3 access (sets AWS_REGION); the bucket's actual region is detected and used if it differs")
	pf.BoolVar(&gf.dryRun, "dry-run", false, "show what would change without transferring anything")
	pf.BoolVar(&gf.force, "force", false, "overwrite a remote file whose content differs from the local one")
	pf.BoolVar(&gf.insecureIgnoreHostKey, "insecure-ignore-host-key", false, "skip SSH host key verification for sftp:// locations (by default an unreadable ~/.ssh/known_hosts, or a host missing from it, is an error)")

	pf.StringVar(&gf.origin, "origin", "", "Origin field recorded in the Release file")
	pf.StringVar(&gf.label, "label", "", "Label field recorded in the Release file")
	pf.StringVar(&gf.codename, "codename", "", "Codename field recorded in the Release file")
	pf.StringVar(&gf.releaseVersion, "release-version", "", "Version field recorded in the Release file")

	pf.BoolVar(&gf.signRelease, "sign-release", false, "GPG-sign the Release file (writes InRelease and Release.gpg)")
	pf.BoolVar(&gf.verifySigs, "verify-sigs", false, "require a trusted signature on a repository being read; a missing key is an error rather than a warning")
	pf.BoolVar(&gf.skipVerify, "skip-verify", false, "do not check the Release signature even when a key is available")
	pf.StringVar(&gf.gpgKey, "gpg-key", "", "path to a GPG private key file (for signing)")
	pf.StringVar(&gf.gpgKeyID, "gpg-key-id", "", "GPG key id/uid from the local keyring (for signing)")
	pf.StringVar(&gf.gpgPass, "gpg-passphrase", os.Getenv("CREATEAPT_GPG_PASSPHRASE"), "passphrase for the signing key (or set CREATEAPT_GPG_PASSPHRASE)")
	pf.StringArrayVar(&gf.keyrings, "keyring", nil, "public keyring file to verify a repository's Release against (repeatable)")

	root.AddCommand(addCmd(), removeCmd(), rebuildCmd(), copyCmd(), createCmd(), listCmd(), verifyCmd(), checkCmd())
	return root
}

// preRunE runs before every subcommand: it resolves the compatibility profile
// and validates that signing-related flags are coherent.
func preRunE(cmd *cobra.Command, args []string) error {
	if err := applyProfile(cmd, args); err != nil {
		return err
	}
	applyAWSEnv()
	backend.InsecureIgnoreHostKey = gf.insecureIgnoreHostKey
	return validateSigningFlags()
}

// applyAWSEnv translates --profile/--region into the AWS_PROFILE/AWS_REGION
// environment variables consulted by the AWS SDK's default config loader. Only
// flags the user actually set are applied, so an existing environment (or the
// default profile) is left untouched otherwise.
//
// os.Setenv only fails on a malformed name, and both names here are constants.
func applyAWSEnv() {
	if gf.awsProfile != "" {
		_ = os.Setenv("AWS_PROFILE", gf.awsProfile)
	}
	if gf.awsRegion != "" {
		_ = os.Setenv("AWS_REGION", gf.awsRegion)
	}
}

// validateSigningFlags rejects ambiguous signing intent. A signing key with no
// instruction about what to sign is almost always a mistake (the key would
// otherwise be silently ignored), so we require the user to be explicit.
func validateSigningFlags() error {
	keyGiven := gf.gpgKey != "" || gf.gpgKeyID != ""
	if keyGiven && !gf.signRelease {
		return fmt.Errorf("a signing key was provided (--gpg-key/--gpg-key-id) but --sign-release was not given; pass it to sign the repository")
	}
	if gf.gpgKey != "" && gf.gpgKeyID != "" {
		return fmt.Errorf("specify only one of --gpg-key or --gpg-key-id")
	}
	if gf.verifySigs && gf.skipVerify {
		return fmt.Errorf("--verify-sigs and --skip-verify contradict each other")
	}
	return nil
}

// applyProfile resolves --target into defaults for index compression and the
// hash set. Flags the user set explicitly are left untouched.
func applyProfile(cmd *cobra.Command, _ []string) error {
	fl := cmd.Flags()
	comp, hashes, err := resolveProfile(gf.target,
		gf.compression, gf.hashes,
		fl.Changed("compression"), fl.Changed("hashes"))
	if err != nil {
		return err
	}
	gf.compression, gf.hashes = comp, hashes
	return nil
}

// resolveProfile applies the named target profile's defaults to compression and
// the hash set, unless the corresponding flag was set explicitly. It is a pure
// function so the profile logic can be tested in isolation.
func resolveProfile(target, compression, hashes string, compressionSet, hashesSet bool) (comp, hash string, err error) {
	if target == "" {
		return compression, hashes, nil
	}
	key := strings.ToLower(target)
	if canon, ok := profileAliases[key]; ok {
		key = canon
	}
	p, ok := profiles[key]
	if !ok {
		return compression, hashes, fmt.Errorf("unknown --target %q (valid: legacy, modern, zstd and distribution aliases such as debian12, jammy)", target)
	}
	if !compressionSet {
		compression = p.compression
	}
	if !hashesSet {
		hashes = p.hashes
	}
	return compression, hashes, nil
}

// repoOptions builds repo.Options from the global flags, attaching a Release
// signer when requested.
func repoOptions(create bool) (repo.Options, error) {
	opts := repo.Options{
		Suite:                      gf.suite,
		Component:                  gf.component,
		Architectures:              splitList(gf.arches),
		FlatPool:                   !gf.poolLayout,
		NoByHash:                   !gf.byHash,
		Validity:                   gf.validity,
		Origin:                     gf.origin,
		Label:                      gf.label,
		Codename:                   gf.codename,
		ReleaseVersion:             gf.releaseVersion,
		Description:                gf.description,
		Create:                     create,
		DryRun:                     gf.dryRun,
		Force:                      gf.force,
		PruneBreakDeps:             gf.pruneBreakDeps,
		RemoveUnreferencedPackages: gf.removeUnreferencedPackages,
		RemoveStaleMetadata:        gf.removeStaleMetadata,
	}

	hashes, err := aptdata.ParseHashes(gf.hashes)
	if err != nil {
		return opts, err
	}
	opts.Hashes = hashes

	comps, err := aptdata.ParseCompressions(gf.compression)
	if err != nil {
		return opts, err
	}
	opts.Compressions = comps

	if gf.signRelease {
		s, err := releaseSigner()
		if err != nil {
			return opts, err
		}
		opts.Signer = s
	}
	return opts, nil
}

func releaseSigner() (repo.Signer, error) {
	switch {
	case gf.gpgKey != "":
		return sign.NewKeyFileSigner(gf.gpgKey, gf.gpgPass)
	case gf.gpgKeyID != "":
		return sign.NewKeyIDSigner(gf.gpgKeyID, gf.gpgPass), nil
	default:
		return nil, fmt.Errorf("--sign-release requires --gpg-key or --gpg-key-id")
	}
}

// ctx returns a context tied to the command.
func ctx(cmd *cobra.Command) context.Context {
	if c := cmd.Context(); c != nil {
		return c
	}
	return context.Background()
}
