package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/progress"
	"github.com/danudey/createapt-go/pkg/repo"
)

func createCmd() *cobra.Command {
	var repoName, repoURL string
	cmd := &cobra.Command{
		Use:   "create <repo-url>",
		Short: "Initialize an empty suite (writes dists/<suite>/)",
		Long: `Initialize an empty suite and record a createapt-go.json config file at the
repository root holding the repository's identity (--repo-name, --repo-url),
the suite and component later operations default to, and the signing settings
in effect. Later add/remove operations reuse those defaults unless overridden.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, prev, err := openRepo(cmd, args[0], true)
			if err != nil {
				return err
			}
			defer r.Close()
			plan, err := r.Commit(ctx(cmd))
			if err != nil {
				return err
			}
			printPlan(cmd, plan, gf.dryRun)
			saveRepoConfig(cmd, r, prev, repoName, repoURL)
			printSourcesLine(cmd, r, baseURLOf(prev, repoURL))
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "repo-name", "", "human-readable repository name to record in the config file")
	cmd.Flags().StringVar(&repoURL, "repo-url", "", "public base URL end users fetch the repository from, recorded in the config file")
	cmd.Flags().StringVar(&gf.description, "description", "", "Description field recorded in the Release file")
	return cmd
}

func removeCmd() *cobra.Command {
	var arch, version string
	cmd := &cobra.Command{
		Use:   "remove <repo-url> <name>...",
		Short: "Remove packages from a suite by name",
		Long: `Remove every package matching the given name(s) from the suite's indexes.
Constrain with --arch (use "source" for source packages) and/or --version. Once
a package is no longer referenced by any index, its file is garbage-collected on
publish.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			location, names := args[0], args[1:]
			r, prev, err := openRepo(cmd, location, false)
			if err != nil {
				return err
			}
			defer r.Close()

			var total int
			for _, n := range names {
				removed := r.Remove(n, arch, version)
				for _, e := range removed {
					fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", e.ID3())
				}
				total += len(removed)
			}
			if total == 0 {
				return fmt.Errorf("no matching packages found")
			}
			printDependencyWarnings(cmd, r)

			plan, err := r.Commit(ctx(cmd))
			if err != nil {
				return err
			}
			printPlan(cmd, plan, gf.dryRun)
			saveRepoConfig(cmd, r, prev, "", "")
			return nil
		},
	}
	cmd.Flags().StringVar(&arch, "arch", "", `only remove packages of this architecture ("source" for source packages)`)
	cmd.Flags().StringVar(&version, "version", "", "only remove this exact version")
	return cmd
}

func listCmd() *cobra.Command {
	var showSources bool
	cmd := &cobra.Command{
		Use:   "list <repo-url>",
		Short: "List packages in a suite",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepoForRead(cmd, args[0])
			if err != nil {
				return err
			}
			defer r.Close()

			out := cmd.OutOrStdout()
			idx := r.Index()
			for _, p := range idx.Packages() {
				fmt.Fprintf(out, "%-44s %-10s %12d  %s\n", p.ID3(), p.Component, p.Size(), p.Location())
			}
			if showSources || idx.HasSources() {
				for _, s := range idx.Sources() {
					fmt.Fprintf(out, "%-44s %-10s %12d  %s/\n", s.ID3()+"_source", s.Component, s.TotalSize(), s.Directory())
				}
			}
			fmt.Fprintf(out, "%d package(s), %d source package(s) in suite %s\n",
				idx.Len(), idx.SourceLen(), r.Suite())
			return nil
		},
	}
	cmd.Flags().BoolVar(&showSources, "sources", false, "list source packages even when there are none (no-op; sources are always listed when present)")
	_ = cmd.Flags().MarkHidden("sources")
	return cmd
}

func verifyCmd() *cobra.Command {
	var checksums bool
	var concurrency int
	cmd := &cobra.Command{
		Use:   "verify <repo-url>",
		Short: "Check that the published files match the indexes",
		Long: `Check that every file the suite's indexes reference is present with the size
and checksum they record, and that the repository's intra-repository
dependencies are satisfied. Nothing is modified.

How far the checksum check goes depends on the backend:

  local disk, SSH/SFTP  the checksum is computed from the stored file (over SSH
                        with sha256sum, so the package never crosses the
                        network), so contents are always proved.
  S3, GCS               the object store reports the checksum recorded when the
                        object was uploaded. That catches indexes and objects
                        disagreeing, but not an object whose content changed
                        afterwards.
  HTTP(S)               no checksum is available without downloading.

Pass --checksums to prove every file's checksum from its content regardless,
downloading each one the backend cannot hash in place. The summary always says
which of the three applied, so an unproved checksum is never reported as though
it had been verified.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepoForRead(cmd, args[0])
			if err != nil {
				return err
			}
			defer r.Close()

			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			mode := repo.ChecksumCheap
			if checksums {
				mode = repo.ChecksumContent
				// A full content check can be a large transfer; say so before
				// starting rather than appearing to hang.
				if n, size := r.DownloadEstimate(); n > 0 {
					fmt.Fprintf(out, "hashing %d file(s) from %s: ~%s to download\n", n, r.Backend(), humanBytes(size))
				}
			}

			res, err := r.Verify(ctx(cmd), repo.VerifyOptions{
				Checksums: mode, Concurrency: concurrency, Bars: bars,
			})
			if err != nil {
				return err
			}
			for _, p := range res.Problems {
				fmt.Fprintln(errOut, "PROBLEM:", p)
			}
			if !res.OK() {
				return fmt.Errorf("%d problem(s) found", len(res.Problems))
			}
			fmt.Fprintln(out, verifySummary(res))
			return nil
		},
	}
	cmd.Flags().BoolVar(&checksums, "checksums", false, "prove every file's checksum from its content, downloading each one the backend cannot hash in place")
	cmd.Flags().IntVar(&concurrency, "concurrency", 6, "number of packages to check in parallel")
	return cmd
}

// verifySummary renders a clean verification result, stating exactly how each
// file's checksum was established so the reader is never left assuming more was
// proved than actually was.
func verifySummary(res *repo.VerifyResult) string {
	var parts []string
	if res.ChecksumVerified > 0 {
		parts = append(parts, fmt.Sprintf("%d verified from content", res.ChecksumVerified))
	}
	if res.ChecksumRecorded > 0 {
		parts = append(parts, fmt.Sprintf("%d matched the store's recorded checksum", res.ChecksumRecorded))
	}
	if res.ChecksumUnconfirmed > 0 {
		parts = append(parts, fmt.Sprintf("%d not confirmed", res.ChecksumUnconfirmed))
	}

	subject := fmt.Sprintf("%d package(s)", res.Packages)
	if res.Sources > 0 {
		subject += fmt.Sprintf(" and %d source package(s)", res.Sources)
	}
	summary := fmt.Sprintf("OK: %s present with the expected size (%d file(s))", subject, res.Files)
	if len(parts) > 0 {
		summary += "; checksums: " + strings.Join(parts, ", ")
	}
	summary += "; dependencies satisfied"
	if res.ChecksumUnconfirmed > 0 {
		summary += "\nRun with --checksums to download and hash the files whose checksum this backend cannot confirm."
	}
	return summary
}

// printPlan renders a commit Plan to the command's output.
// dryRunPrefix marks every line of output that describes an action the run did
// not actually take.
const dryRunPrefix = "[dry-run] "

func printPlan(cmd *cobra.Command, plan *repo.Plan, dryRun bool) {
	out := cmd.OutOrStdout()
	prefix := ""
	if dryRun {
		prefix = dryRunPrefix
	}
	for _, a := range plan.Uploads {
		fmt.Fprintf(out, "%supload  %s (%d bytes) — %s\n", prefix, a.Location, a.Size, a.Reason)
	}
	for _, a := range plan.Skipped {
		// A file the copy command already transferred was reported as it went
		// past; repeating it here would double every line of a mirror's output.
		// It still counts towards the summary below.
		if a.Reason == repo.ReasonCopied {
			continue
		}
		fmt.Fprintf(out, "%sskip    %s — %s\n", prefix, a.Location, a.Reason)
	}
	// A relocation's source is also listed in DeletedFiles (it is removed after
	// the copy); render it as a single "move" line and suppress the plain
	// "delete" line for it.
	movedFrom := make(map[string]bool, len(plan.Copies))
	for _, c := range plan.Copies {
		fmt.Fprintf(out, "%smove    %s -> %s (server-side, no upload)\n", prefix, c.From, c.To)
		movedFrom[c.From] = true
	}
	for _, p := range plan.DeletedFiles {
		if movedFrom[p] {
			continue
		}
		fmt.Fprintf(out, "%sdelete  %s\n", prefix, p)
	}
	for _, p := range plan.UnreferencedFiles {
		fmt.Fprintf(out, "%sdelete  %s (unreferenced)\n", prefix, p)
	}
	for _, p := range plan.ObsoleteMeta {
		fmt.Fprintf(out, "%sgc      %s\n", prefix, p)
	}
	for _, p := range plan.StaleMetadata {
		fmt.Fprintf(out, "%sgc      %s (stale)\n", prefix, p)
	}

	signed := ""
	if plan.Signed {
		signed = ", Release signed"
	}
	moved := ""
	if len(plan.Copies) > 0 {
		moved = fmt.Sprintf(", %d relocated", len(plan.Copies))
	}
	sources := ""
	if plan.Sources > 0 {
		sources = fmt.Sprintf(", %d source package(s)", plan.Sources)
	}
	fmt.Fprintf(out, "%s%d package(s)%s; %d upload(s) (%s), %d skipped%s%s; %d index file(s)\n",
		prefix, plan.Packages, sources, len(plan.Uploads), humanBytes(plan.BytesToUpload),
		len(plan.Skipped), moved, signed, len(plan.MetadataFiles))
}

// printPruneWarnings reports dependency breakages from a prune. kept lists
// versions retained because a dependent needs them (the default); broken lists
// versions dropped despite a dependent (--prune-break-deps). Warnings go to
// stderr so they stand out from the normal plan output.
func printPruneWarnings(cmd *cobra.Command, kept, broken []aptdata.Breakage) {
	errOut := cmd.ErrOrStderr()
	for _, b := range kept {
		fmt.Fprintf(errOut, "warning: keeping %s: it is required by %s (%s)\n",
			b.Provider.ID3(), b.Dependent.ID3(), b.Requires.String())
	}
	for _, b := range broken {
		fmt.Fprintf(errOut, "warning: pruned %s though %s depends on %s (--prune-break-deps)\n",
			b.Provider.ID3(), b.Dependent.ID3(), b.Requires.String())
	}
}

// printDependencyWarnings reports intra-repository dependencies the current
// index leaves unsatisfied. A removal is allowed to break the graph — the
// operator may be mid-way through a larger change — but it is never silent.
func printDependencyWarnings(cmd *cobra.Command, r *repo.Repo) {
	errOut := cmd.ErrOrStderr()
	for _, p := range r.CheckDependencies() {
		fmt.Fprintf(errOut, "warning: %s\n", p)
	}
}

// printSourcesLine prints the sources.list entry a client needs. It is the one
// piece of information a user always has to assemble by hand after publishing,
// and it can only be printed when the repository's public URL is known.
func printSourcesLine(cmd *cobra.Command, r *repo.Repo, baseURL string) {
	if baseURL == "" {
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nAdd this repository with:\n  deb %s %s %s\n",
		strings.TrimRight(baseURL, "/"), r.Suite(), effectiveComponent())
}

// humanBytes renders a byte count in the largest unit that keeps it readable.
// The progress display formats the same figures, so both read alike.
func humanBytes(n int64) string { return progress.HumanBytes(n) }
