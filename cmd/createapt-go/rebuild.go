package main

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/repo"
)

func rebuildCmd() *cobra.Command {
	var pruneOlder, assumeYes, fromPackages bool
	cmd := &cobra.Command{
		Use:   "rebuild <repo-url>",
		Short: "Reconcile an existing suite against updated options",
		Long: `Load an existing suite's indexes, apply updated options, reconcile the new
state, and republish.

When the packages are local files, rebuild re-reads every .deb and regenerates
its index stanza from the file, so a package that was replaced under the same
name stops being described by the checksum, size and dependencies of the build
it superseded. Reading them is free in that case, so it is the default. When the
packages live on remote storage, reading them means downloading the repository,
so rebuild does not do it unless asked with --from-packages — and says clearly
that it is republishing the indexes it was given rather than checking them.

Apart from that, rebuild never downloads packages: it can prune superseded
versions (--prune-older), move every package to match a changed --pool-layout
(relocated server-side, no re-upload), change the index compression or hash set,
and re-sign the Release when a signing key is configured.

Opt-in extras:
  --remove-unreferenced-packages delete pool files the indexes no longer
                                 reference.
  --remove-stale-metadata        delete index files under dists/ no longer in
                                 use.

rebuild always prints a before/after summary and asks for confirmation before
making changes. Pass --yes to skip the prompt, or --dry-run to only report.
The cleanup flags require a backend that can list its contents (not plain HTTP).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			location := args[0]
			r, prev, err := openRepo(cmd, location, false)
			if err != nil {
				return err
			}
			defer r.Close()

			// Snapshot what was published before anything is reconciled, so the
			// report contrasts it with the result — including any correction
			// the refresh below makes.
			beforeEntries := r.Index().Entries()
			beforeCount := len(beforeEntries)
			var beforeBytes int64
			for _, e := range beforeEntries {
				beforeBytes += e.TotalBytes()
			}

			// Re-read the packages before reconciling, so the prune's version
			// comparisons and every number below describe what is actually
			// stored rather than what the old indexes claimed.
			refreshed, err := refreshFromPackages(cmd, r, fromPackages)
			if err != nil {
				return err
			}
			totalBytes, totalFiles, listed, err := r.TotalSize(ctx(cmd))
			if err != nil {
				return err
			}

			// Reconcile the index in memory — no downloads.
			var pruneRep repo.PruneReport
			if pruneOlder {
				pruneRep = r.PruneOlderVersions()
				printPruneWarnings(cmd, pruneRep.Kept, pruneRep.Broken)
			}
			relocated := r.RelocateAll()

			// Preview the plan (no side effects), including directory cleanups.
			preview, err := r.Plan(ctx(cmd))
			if err != nil {
				return err
			}

			afterEntries := r.Index().Entries()
			var afterBytes int64
			for _, e := range afterEntries {
				afterBytes += e.TotalBytes()
			}

			printRebuildReport(cmd, rebuildReport{
				location:    location,
				suite:       r.Suite(),
				beforeCount: beforeCount,
				afterCount:  len(afterEntries),
				beforeBytes: beforeBytes,
				afterBytes:  afterBytes,
				totalBytes:  totalBytes,
				totalFiles:  totalFiles,
				sizeListed:  listed,
				relocated:   relocated,
				refreshed:   refreshed,
				freedBytes:  deletedBytes(beforeEntries, preview),
				plan:        preview,
				depProblems: r.CheckDependencies(),
			})

			if gf.dryRun {
				printPlan(cmd, preview, true)
				return nil
			}

			// Destructive from here on (deletes): confirm unless the operator
			// opted out.
			if !assumeYes {
				if !promptYesNo(cmd, "Proceed with these changes?") {
					fmt.Fprintln(cmd.OutOrStdout(), "aborted; no changes made")
					return nil
				}
			}

			plan, err := r.Commit(ctx(cmd))
			if err != nil {
				return err
			}
			printPlan(cmd, plan, false)
			saveRepoConfig(cmd, r, prev, "", "")
			return nil
		},
	}
	cmd.Flags().BoolVar(&fromPackages, "from-packages", false, "re-read every .deb and regenerate its index stanza from the file (default: on when the packages are local files, off when reading them means downloading)")
	cmd.Flags().BoolVar(&pruneOlder, "prune-older", false, "drop superseded versions, keeping only the newest of each name+architecture")
	cmd.Flags().BoolVar(&gf.pruneBreakDeps, "prune-break-deps", false, "when pruning, drop a version even if another package depends on it (default: keep it and warn)")
	cmd.Flags().BoolVar(&gf.removeUnreferencedPackages, "remove-unreferenced-packages", false, "delete pool files the indexes no longer reference")
	cmd.Flags().BoolVar(&gf.removeStaleMetadata, "remove-stale-metadata", false, "delete index files under dists/ no longer in use")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// refreshFromPackages re-derives every package's index stanza from its .deb,
// which is what makes a rebuild correct a repository whose package files were
// replaced underneath the indexes. It is on by default when the packages are
// local files, because re-reading them costs nothing; when reading them means
// downloading the whole repository it is off unless asked for, and rebuild says
// plainly that it is publishing the indexes it was given rather than checking
// them. It returns nil when no refresh was performed.
func refreshFromPackages(cmd *cobra.Command, r *repo.Repo, requested bool) (*repo.RefreshResult, error) {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	refresh := r.PackagesAreLocal()
	if cmd.Flags().Changed("from-packages") {
		refresh = requested
	}

	if !refresh {
		fmt.Fprintf(errOut, "warning: the indexes are being regenerated from the published index, not re-read from the package files, "+
			"so anything the index gets wrong about a package (checksum, size, dependencies) stays wrong. "+
			"Pass --from-packages to re-read them")
		if n, size := r.RefreshEstimate(); n > 0 {
			fmt.Fprintf(errOut, ", which downloads %d package(s) (~%s)", n, humanBytes(size))
		}
		fmt.Fprintln(errOut, ".")
		return nil, nil
	}

	if n, size := r.RefreshEstimate(); n > 0 {
		fmt.Fprintf(out, "re-reading %d package(s) from %s: ~%s to download\n", n, r.Backend(), humanBytes(size))
	}
	res, err := r.RefreshFromPackages(ctx(cmd), repo.RefreshOptions{
		Progress: func(p *aptdata.Package, changed bool) {
			if changed {
				fmt.Fprintf(out, "refreshed %s from its .deb (the published index did not match the file)\n", p.ID3())
			}
		},
	})
	if err != nil {
		return nil, err
	}
	for _, loc := range res.Missing {
		fmt.Fprintf(errOut, "warning: %s is referenced by the indexes but not present; its entry is left as published\n", loc)
	}
	for _, bad := range res.Unreadable {
		fmt.Fprintf(errOut, "warning: could not read %s as a Debian package; its entry is left as published\n", bad)
	}
	for _, c := range res.Conflicts {
		fmt.Fprintf(errOut, "warning: %s; the indexes can only name one of them, so both entries are left as published. "+
			"Remove one of the files if the duplication is unintended\n", c)
	}
	return res, nil
}

// deletedBytes sums the size of the pool files a plan will delete, taking each
// size from the indexes as they were published. Stray files the indexes never
// named (--remove-unreferenced-packages) have no recorded size and are not
// counted, so the figure is a floor.
func deletedBytes(before []aptdata.Entry, plan *repo.Plan) int64 {
	if plan == nil {
		return 0
	}
	size := map[string]int64{}
	for _, e := range before {
		switch t := e.(type) {
		case *aptdata.Package:
			size[t.Location()] = t.Size()
		case *aptdata.Source:
			dir := t.Directory()
			for _, f := range t.SourceFiles() {
				size[f.Location(dir)] = f.Size
			}
		}
	}
	var total int64
	for _, p := range plan.DeletedFiles {
		total += size[p]
	}
	return total
}

// rebuildReport carries the numbers shown before a rebuild is confirmed.
type rebuildReport struct {
	location    string
	suite       string
	beforeCount int
	afterCount  int
	beforeBytes int64
	afterBytes  int64
	totalBytes  int64
	totalFiles  int
	sizeListed  bool
	relocated   int
	refreshed   *repo.RefreshResult
	freedBytes  int64
	plan        *repo.Plan
	depProblems []aptdata.DependencyProblem
}

// printRebuildReport renders the before/after summary. Only lines with
// something to report are shown.
func printRebuildReport(cmd *cobra.Command, rep rebuildReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Rebuild plan for %s (suite %s)\n", rep.location, rep.suite)
	fmt.Fprintf(out, "  indexed entries:     %d -> %d (%+d)\n",
		rep.beforeCount, rep.afterCount, rep.afterCount-rep.beforeCount)
	fmt.Fprintf(out, "  package payload:     %s -> %s\n",
		humanBytes(rep.beforeBytes), humanBytes(rep.afterBytes))
	approx := ""
	if !rep.sizeListed {
		approx = " (estimated from the indexes; backend cannot list)"
	}
	fmt.Fprintf(out, "  repository on disk:  %s across %d object(s)%s\n",
		humanBytes(rep.totalBytes), rep.totalFiles, approx)
	// Show the estimated new total when files are actually being deleted. This
	// counts the files the plan removes, not the drop in package payload: a
	// duplicate index entry for a file another entry still names shrinks the
	// payload without freeing a byte.
	if rep.freedBytes > 0 {
		fmt.Fprintf(out, "  after rebuild (est.): %s (frees ~%s)\n",
			humanBytes(rep.totalBytes-rep.freedBytes), humanBytes(rep.freedBytes))
	}

	if f := rep.refreshed; f != nil {
		fmt.Fprintf(out, "  re-read from .debs:  %d package(s)", f.Read)
		if f.Changed > 0 {
			fmt.Fprintf(out, ", %d corrected (the index did not match the file)", f.Changed)
		}
		if f.Duplicates > 0 {
			fmt.Fprintf(out, ", %d duplicate record(s) dropped", f.Duplicates)
		}
		fmt.Fprintln(out)
	}
	if rep.relocated > 0 {
		fmt.Fprintf(out, "  relocate:            %d entr(ies) moved server-side\n", rep.relocated)
	}
	if p := rep.plan; p != nil {
		if len(p.Uploads) > 0 {
			fmt.Fprintf(out, "  upload:              %d file(s) (%s)\n", len(p.Uploads), humanBytes(p.BytesToUpload))
		}
		if len(p.DeletedFiles) > 0 {
			fmt.Fprintf(out, "  gc packages:         %d no-longer-referenced file(s)\n", len(p.DeletedFiles))
		}
		if len(p.UnreferencedFiles) > 0 {
			fmt.Fprintf(out, "  remove unreferenced: %d stray pool file(s)\n", len(p.UnreferencedFiles))
		}
		if len(p.StaleMetadata) > 0 {
			fmt.Fprintf(out, "  remove stale meta:   %d index file(s)\n", len(p.StaleMetadata))
		}
	}
	if len(rep.depProblems) > 0 {
		fmt.Fprintf(out, "  broken deps:         %d unmet intra-repo dependenc(ies) after rebuild:\n", len(rep.depProblems))
		for _, dp := range rep.depProblems {
			fmt.Fprintf(out, "                       %s\n", dp)
		}
	}
}

// promptYesNo asks question on stdout and reads a line from the command's
// input, returning true only for an explicit yes. EOF or a blank line means no.
func promptYesNo(cmd *cobra.Command, question string) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", question)
	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false // EOF with no input: treat as "no"
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}
