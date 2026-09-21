package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danudey/createapt-go/pkg/debmeta"
)

func addCmd() *cobra.Command {
	var prune bool
	cmd := &cobra.Command{
		Use:   "add <repo-url> <package-or-dir>...",
		Short: "Add (or update) packages in a suite",
		Long: `Upload one or more packages and add them to the live suite indexes, creating
the suite if it does not yet exist.

Each positional argument may be a .deb (or .udeb) file, a .dsc source control
file, or a directory. Directories are scanned recursively and every .deb and
.dsc found within them is added. A .dsc's tarballs are uploaded alongside it and
are checked against the checksums the .dsc records before anything is
transferred.

A package whose name+version+architecture already exists is replaced. With
--prune-older, older versions of the same name+architecture are also removed.
Whenever a file stops being referenced by the indexes — because it was replaced,
pruned, or moved to a new location — it is garbage-collected on publish. A
relocated package (identical content at a new location) is moved server-side
rather than re-uploaded when the backend supports it.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			location := args[0]
			files, err := expandPackageArgs(args[1:])
			if err != nil {
				return err
			}

			// Open the repo first so persisted defaults (suite, component,
			// pool layout, signing key) apply to the packages added below.
			r, prev, err := openRepo(cmd, location, true)
			if err != nil {
				return err
			}
			defer r.Close()
			r.SetPruneOlder(prune)

			out := cmd.OutOrStdout()
			for _, p := range files {
				entry, err := r.Add(p)
				if err != nil {
					return fmt.Errorf("add %s: %w", p, err)
				}
				fmt.Fprintf(out, "staged %s -> %s\n", entry.ID3(), strings.Join(entry.Locations(), ", "))
			}
			kept, broken := r.PruneWarnings()
			printPruneWarnings(cmd, kept, broken)
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
	cmd.Flags().BoolVar(&prune, "prune-older", false, "remove older versions of the same name+architecture")
	cmd.Flags().BoolVar(&gf.pruneBreakDeps, "prune-break-deps", false, "when pruning, drop a version even if another package depends on it (default: keep it and warn)")
	return cmd
}

// expandPackageArgs turns the positional arguments — each a package file or a
// directory — into a deduplicated, sorted list of file paths to add.
// Directories are walked recursively and only files with a recognized extension
// are collected; an explicitly named file is kept as given (so a caller can add
// a package whose name does not end in .deb). It returns an error if a path
// does not exist or if no files were found.
func expandPackageArgs(paths []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		key := p
		if abs, err := filepath.Abs(p); err == nil {
			key = abs
		}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, p)
	}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if !info.IsDir() {
			add(p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && (debmeta.IsDeb(path) || debmeta.IsDSC(path)) {
				add(path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", p, err)
		}
	}

	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("no .deb or .dsc files found in the given path(s)")
	}
	return sortSourcesLast(out), nil
}

// sortSourcesLast puts .dsc files after binary packages.
//
// A .dsc names tarballs that share a pool directory with the binaries built
// from the same source. Adding the binaries first means the pool directory is
// already established when the source's files land in it, which keeps the
// staged upload order the same as the published layout and makes the run's
// output read in the order a reader expects.
func sortSourcesLast(paths []string) []string {
	binaries := make([]string, 0, len(paths))
	var sources []string
	for _, p := range paths {
		if debmeta.IsDSC(p) {
			sources = append(sources, p)
			continue
		}
		binaries = append(binaries, p)
	}
	return append(binaries, sources...)
}
