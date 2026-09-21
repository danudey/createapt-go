package main

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/danudey/createapt-go/pkg/repocheck"
)

// errCheckFailed is returned when at least one check failed. It carries no
// message of its own: the failures have already been printed in detail, and a
// second summary line would only repeat them.
var errCheckFailed = errors.New("validation failed")

// checkFlags holds the check-specific options.
type checkFlags struct {
	sourcesFile string
	level       string
	suite       string
	component   string
	arch        string
	packages    string
	versions    string
	concurrency int
	timeout     time.Duration
	verbose     bool
}

func checkCmd() *cobra.Command {
	var cf checkFlags
	cmd := &cobra.Command{
		Use:   "check [repo-url]",
		Short: "Deep-validate a repository's metadata and packages",
		Long: `Validate an apt repository layer by layer: read the suite's Release file,
check every index it covers against the size and checksums recorded there, parse
those indexes, and confirm the packages they reference exist with the size and
checksum claimed.

This catches a repository where a published package file has diverged from what
the indexes describe — one .deb published over another without regenerating the
metadata, say — which end users see as "Hash Sum mismatch" on install.

The positional argument (or --sources-file) may be a repository location — a
local path, or a file://, sftp://, s3://, gs:// or http(s):// URL — or a
path/URL to a sources file (ending in .list or .sources), in which case every
entry it names is checked as its own target.

Levels (each includes the previous):
  metadata  the Release is present and parseable, and every index it references
            reads back with the correct size and checksums and parses.
  head      ...plus every package exists with the size the index claims (and,
            when the backend can checksum without transferring the file — S3,
            GCS, SFTP — the correct checksum).
  fetch     ...plus every package is downloaded, its size and checksum are
            verified from its content, and its archive is opened and parsed.

Examples:
  # HEAD-check every package in a live repository:
  createapt-go check https://deb.example.com/apt --suite bookworm

  # Check every entry a sources file names:
  createapt-go check /etc/apt/sources.list.d/example.sources

  # Fully download and verify a local repository:
  createapt-go check --level fetch /srv/repo

  # Checksum-verify just two packages on S3 without a full sweep:
  createapt-go check --level head --packages hello,libfoo s3://my-bucket/apt

  # Only validate that a GCS repository's indexes are internally consistent:
  createapt-go check --level metadata gs://my-bucket/apt`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input := cf.sourcesFile
			if len(args) == 1 {
				if input != "" {
					return fmt.Errorf("give either a positional repository/sources argument or --sources-file, not both")
				}
				input = args[0]
			}
			if input == "" {
				return fmt.Errorf("a repository location or --sources-file is required")
			}

			level, err := repocheck.ParseLevel(cf.level)
			if err != nil {
				return err
			}
			latestOnly, err := resolveVersions(cf.versions, level)
			if err != nil {
				return err
			}

			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

			var logf func(format string, args ...any)
			if cf.verbose {
				logf = func(format string, a ...any) { fmt.Fprintf(errOut, format+"\n", a...) }
			}

			deb := repocheck.DetectDebTool()
			fmt.Fprintf(out, "check: level=%s versions=%s\n", level, versionsLabel(latestOnly))

			results, warnings, err := repocheck.Run(ctx(cmd), repocheck.Options{
				Input:       input,
				Suites:      suitesFor(cf.suite),
				Components:  splitList(cf.component),
				Arches:      resolveArches(cf.arch),
				Packages:    splitList(cf.packages),
				Level:       level,
				LatestOnly:  latestOnly,
				Concurrency: cf.concurrency,
				Timeout:     cf.timeout,
				Deb:         deb,
				Logf:        logf,
				OnTargetStart: func(t repocheck.Target) {
					fmt.Fprintf(out, "\n== %s ==\n   %s (suite %s)\n", t.Label, t.BaseURL, t.Suite)
				},
			})
			for _, w := range warnings {
				fmt.Fprintln(errOut, "warning:", w)
			}
			if err != nil {
				return err
			}

			if failed := reportCheck(out, results); failed {
				return errCheckFailed
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cf.sourcesFile, "sources-file", "", "path or URL to a sources.list (.list) or deb822 (.sources) file (alternative to the positional argument)")
	cmd.Flags().StringVar(&cf.level, "level", "head", "validation depth: metadata|head|fetch")
	cmd.Flags().StringVar(&cf.suite, "suite", "", "comma-separated suites to check (default: the repository's recorded suite, or stable)")
	cmd.Flags().StringVar(&cf.component, "component", "", "comma-separated components to check (empty means every component the Release lists)")
	cmd.Flags().StringVar(&cf.arch, "arch", "", `comma-separated architectures to check (e.g. amd64,arm64). Empty or "host" means this machine's; "any" checks every architecture in the indexes; name "source" to include source packages`)
	cmd.Flags().StringVar(&cf.packages, "packages", "", "comma-separated package names to check (empty means all)")
	cmd.Flags().StringVar(&cf.versions, "versions", "", "which versions to check: latest|all (default: latest for fetch, all for metadata/head)")
	cmd.Flags().IntVar(&cf.concurrency, "concurrency", 6, "number of packages to check in parallel")
	cmd.Flags().DurationVar(&cf.timeout, "timeout", 60*time.Second, "per-operation timeout (0 to disable)")
	cmd.Flags().BoolVarP(&cf.verbose, "verbose", "v", false, "print a line for every check, not just failures and warnings")
	return cmd
}

// resolveVersions decides whether only the newest version of each package is
// checked. The default depends on the level: a fetch downloads everything it
// checks, so it defaults to the latest version only; the cheaper levels check
// everything.
func resolveVersions(flagVal string, level repocheck.Level) (latestOnly bool, err error) {
	switch strings.ToLower(strings.TrimSpace(flagVal)) {
	case "":
		return level == repocheck.LevelFetch, nil
	case "latest":
		return true, nil
	case versionsAll:
		return false, nil
	default:
		return false, fmt.Errorf("invalid --versions %q (want latest|all)", flagVal)
	}
}

// reportCheck prints failures/warnings and a summary. It returns true if at
// least one check failed.
func reportCheck(out io.Writer, results []repocheck.Result) bool {
	oks, fails, warns, skips := repocheck.Summarize(results)

	// Sort failures/warnings to the top of a detail listing.
	sorted := make([]repocheck.Result, len(results))
	copy(sorted, results)
	sort.SliceStable(sorted, func(i, j int) bool {
		return statusRank(sorted[i].Status) < statusRank(sorted[j].Status)
	})

	if fails > 0 || warns > 0 {
		fmt.Fprintf(out, "\nIssues:\n")
		for _, r := range sorted {
			if r.Status == repocheck.StatusFail || r.Status == repocheck.StatusWarn {
				fmt.Fprintf(out, "  [%s] %s :: %s\n        %s\n        %s\n", r.Status, r.Target, r.Kind, r.Detail, r.Loc)
			}
		}
	}

	fmt.Fprintf(out, "\nSummary: %d ok, %d failed, %d warnings, %d skipped\n", oks, fails, warns, skips)
	if fails > 0 {
		fmt.Fprintln(out, "RESULT: FAIL")
		return true
	}
	fmt.Fprintln(out, "RESULT: OK")
	return false
}

func statusRank(s repocheck.Status) int {
	switch s {
	case repocheck.StatusFail:
		return 0
	case repocheck.StatusWarn:
		return 1
	case repocheck.StatusSkip:
		return 2
	default:
		return 3
	}
}

// versionsAll is the --versions value meaning "every version".
const versionsAll = "all"

func versionsLabel(latestOnly bool) string {
	if latestOnly {
		return "latest"
	}
	return versionsAll
}

// resolveArches turns the --arch flag into a concrete list. Empty or "host"
// means this machine's architecture; "any" means do not filter by architecture.
func resolveArches(flagVal string) []string {
	switch strings.ToLower(strings.TrimSpace(flagVal)) {
	case "any":
		return nil
	case "", "host":
		return []string{hostArch()}
	default:
		return splitList(flagVal)
	}
}

// suitesFor splits the --suite flag, which accepts a comma-separated list so
// several suites of one repository can be checked in a single run.
func suitesFor(flagVal string) []string { return splitList(flagVal) }

// hostArch maps the Go architecture name to Debian's.
func hostArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "386":
		return "i386"
	case "arm":
		return "armhf"
	case "ppc64le":
		return "ppc64el"
	case "s390x":
		return "s390x"
	case "riscv64":
		return "riscv64"
	default:
		return runtime.GOARCH
	}
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
