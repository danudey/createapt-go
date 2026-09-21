package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/backend"
	"github.com/danudey/createapt-go/pkg/repo"
	"github.com/danudey/createapt-go/pkg/repoconfig"
)

// copyExact replicates the source suite file by file. Nothing is regenerated,
// so the destination is byte-identical to the source and any Release signature
// it carries stays valid.
func copyExact(cmd *cobra.Command, src *repo.Repo, srcLoc string, srcCfg *repoconfig.Config,
	dstBE backend.Backend, dstLoc string, dstExists bool, sourceSigned bool, cf *copyFlags,
) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	defer closeBackend(dstBE)

	// An exact copy republishes the source's own Release, so the copy is signed
	// exactly when the source was. Only a copy that would be unsigned and is
	// not about to be re-signed has to account for that.
	if !sourceSigned && !signingKeyGiven() {
		if err := requireSigningIntent(errOut); err != nil {
			return err
		}
	}

	objs, listed, err := src.Objects(ctx(cmd))
	if err != nil {
		return err
	}
	if !listed {
		fmt.Fprintf(errOut, "warning: %s cannot list its contents, so only the files the metadata references are copied\n", src.Backend())
	}

	prune := cf.overwrite && dstExists
	fmt.Fprintf(out, "Copy %s -> %s (exact, suite %s, %d object(s), %s)\n",
		srcLoc, dstLoc, src.Suite(), len(objs), humanBytes(totalObjectSize(objs)))
	if prune {
		fmt.Fprintf(out, "  --overwrite: files at the destination that the source does not have will be deleted\n")
		if !cf.assumeYes && !gf.dryRun {
			if !promptYesNo(cmd, "Proceed?") {
				fmt.Fprintln(out, "aborted; no changes made")
				return nil
			}
		}
	}

	prefix := ""
	if gf.dryRun {
		prefix = dryRunPrefix
	}
	stats, err := repo.CopyExact(ctx(cmd), src.Backend(), dstBE, objs, repo.CopyOptions{
		DryRun: gf.dryRun,
		Force:  gf.force,
		Prune:  prune,
		Progress: func(action string, obj repo.SourceObject) {
			fmt.Fprintf(out, "%s%-7s %s\n", prefix, action, obj.Path)
		},
	})
	if err != nil {
		return err
	}

	if err := resignCopiedRelease(cmd, src, dstBE); err != nil {
		return err
	}

	// The source's config file came across verbatim; patch in this copy's
	// origin and identity.
	cfg := repoconfig.Config{}
	if srcCfg != nil {
		cfg = *srcCfg
	}
	cfg.Suite = src.Suite()
	applyCopyConfig(&cfg, srcLoc, cf)
	writeRepoConfig(cmd, dstBE, &cfg)

	fmt.Fprintf(out, "%s%d object(s) copied (%s), %d already present%s\n",
		prefix, stats.Copied, humanBytes(stats.Bytes), stats.Skipped, deletedSuffix(stats))
	return nil
}

// resignCopiedRelease replaces the copied Release signatures with ones made by
// this operator's key, when one was given. The copied Release is
// byte-identical to the source's, so the new signature is over the same
// document the source signed. With no key of our own the source's signature is
// what the copy carries, which is the point of an exact copy.
func resignCopiedRelease(cmd *cobra.Command, src *repo.Repo, dstBE backend.Backend) error {
	if !gf.signRelease || !signingKeyGiven() {
		return nil
	}
	signer, err := releaseSigner()
	if err != nil {
		return err
	}
	raw, err := src.RawRelease(ctx(cmd))
	if err != nil {
		return fmt.Errorf("reading the source's Release to re-sign it: %w", err)
	}
	detached, err := signer.SignDetached(raw)
	if err != nil {
		return fmt.Errorf("sign Release: %w", err)
	}
	inline, err := signer.SignClearsigned(raw)
	if err != nil {
		return fmt.Errorf("clearsign Release: %w", err)
	}

	suite := src.Suite()
	out := cmd.OutOrStdout()
	if gf.dryRun {
		fmt.Fprintf(out, "[dry-run] sign    %s, %s\n", aptdata.ReleaseGPGPath(suite), aptdata.InReleasePath(suite))
		return nil
	}
	for _, spec := range []struct {
		path string
		data []byte
	}{
		{aptdata.ReleaseGPGPath(suite), detached},
		{aptdata.InReleasePath(suite), inline},
	} {
		if err := dstBE.Put(ctx(cmd), spec.path, bytes.NewReader(spec.data), int64(len(spec.data))); err != nil {
			return fmt.Errorf("write %s: %w", spec.path, err)
		}
		fmt.Fprintf(out, "signed  %s\n", spec.path)
	}
	return nil
}

// copyRun carries the state a rebuilding copy needs.
type copyRun struct {
	src          *repo.Repo
	srcLoc       string
	srcCfg       *repoconfig.Config
	sourceSigned bool

	dstBE     backend.Backend
	dstLoc    string
	dstExists bool
	dstSuite  string

	selected []aptdata.Entry
	cf       *copyFlags

	relocate bool

	out    io.Writer
	errOut io.Writer
}

// copyRebuilding copies the selected packages and regenerates the destination's
// indexes around them. It is the path taken whenever an option makes the copy
// something other than a verbatim replica.
func copyRebuilding(cmd *cobra.Command, run copyRun) error {
	cf := run.cf
	dst, prev, err := openRepoBackend(cmd, run.dstBE, true)
	if err != nil {
		return err
	}
	defer dst.Close()

	// Rebuilt indexes are a different document from the one the source signed,
	// so a signed source may only be transformed if the copy gets a signature
	// of its own.
	signingCopy := gf.signRelease && signingKeyGiven()
	if run.sourceSigned && !signingCopy {
		return fmt.Errorf("this copy rebuilds the indexes (%s), which invalidates the signature the source carries; "+
			"re-sign the copy with --gpg-key or --gpg-key-id, or copy the repository unchanged",
			strings.Join(transformReasons(run), ", "))
	}

	if cf.overwrite {
		existing := dst.Index().Total()
		dst.ClearIndex()
		if run.dstExists && !cf.assumeYes && !gf.dryRun {
			fmt.Fprintf(run.out, "--overwrite: the %d package(s) already in %s at %s are dropped, and any pool file the new indexes do not reference is deleted\n",
				existing, run.dstSuite, run.dstLoc)
			if !promptYesNo(cmd, "Proceed?") {
				fmt.Fprintln(run.out, "aborted; no changes made")
				return nil
			}
		}
	}

	prefix := ""
	if gf.dryRun {
		prefix = dryRunPrefix
	}
	fmt.Fprintf(run.out, "Copy %s -> %s (rebuilding indexes, suite %s, %d package(s))\n",
		run.srcLoc, run.dstLoc, run.dstSuite, len(run.selected))

	stats, err := dst.CopyEntriesFrom(ctx(cmd), run.src.Backend(), run.selected, repo.EntryCopyOptions{
		Relocate:        run.relocate || cf.dstComponent != "",
		Component:       cf.dstComponent,
		RebuildMetadata: cf.rebuildMetadata,
		Progress: func(action string, e aptdata.Entry, loc string) {
			fmt.Fprintf(run.out, "%s%-7s %s -> %s\n", prefix, action, e.ID3(), loc)
		},
	})
	if err != nil {
		return err
	}

	// Pruning reconciles the destination once every copied package is in the
	// index, so it also drops versions the destination already held.
	if cf.pruneOlder {
		rep := dst.PruneOlderVersions()
		printPruneWarnings(cmd, rep.Kept, rep.Broken)
		for _, e := range rep.Removed {
			fmt.Fprintf(run.out, "%spruned  %s\n", prefix, e.ID3())
		}
	}

	plan, err := dst.Commit(ctx(cmd))
	if err != nil {
		return err
	}
	printPlan(cmd, plan, gf.dryRun)
	for _, dp := range dst.CheckDependencies() {
		fmt.Fprintf(run.errOut, "warning: unmet dependency after copy: %s\n", dp)
	}

	cfg := effectiveConfig(cmd, prev, "", "")
	if run.srcCfg != nil {
		if cfg.Name == "" {
			cfg.Name = run.srcCfg.Name
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = run.srcCfg.BaseURL
		}
	}
	applyCopyConfig(&cfg, run.srcLoc, cf)
	writeRepoConfig(cmd, dst.Backend(), &cfg)

	fmt.Fprintf(run.out, "%s%d file(s) transferred (%s), %d already present\n",
		prefix, stats.Copied, humanBytes(stats.Bytes), stats.Skipped)
	return nil
}

// applyCopyConfig records the copy's origin in the destination config and
// applies the identity the operator gave it.
func applyCopyConfig(cfg *repoconfig.Config, srcLoc string, cf *copyFlags) {
	if cf.repoName != "" {
		cfg.Name = cf.repoName
	}
	if cf.repoURL != "" {
		cfg.BaseURL = cf.repoURL
	}
	cfg.CopySource = srcLoc
}

// transformReasons lists, for the error message, the options that made this
// copy rebuild the indexes rather than replicate them.
func transformReasons(run copyRun) []string {
	cf := run.cf
	var why []string
	add := func(cond bool, name string) {
		if cond {
			why = append(why, name)
		}
	}
	add(cf.update, "--update")
	add(cf.rebuildMetadata, "--rebuild-metadata")
	add(cf.pruneOlder, "--prune-older")
	add(cf.latestOnly, "--latest-only")
	add(len(cf.include) > 0, "--include")
	add(len(cf.exclude) > 0, "--exclude")
	add(len(cf.arches) > 0, "--arch")
	add(cf.kinds != "", "--kinds")
	add(cf.excludeKinds != "", "--exclude-kinds")
	add(run.relocate, "--pool-layout")
	add(cf.dstSuite != "" && cf.dstSuite != run.src.Suite(), "--to-suite")
	add(cf.dstComponent != "", "--to-component")
	if len(why) == 0 {
		why = append(why, "the requested changes")
	}
	return why
}

func totalObjectSize(objs []repo.SourceObject) int64 {
	var total int64
	for _, o := range objs {
		if o.Size > 0 {
			total += o.Size
		}
	}
	return total
}

func deletedSuffix(stats *repo.CopyStats) string {
	if len(stats.Deleted) == 0 {
		return ""
	}
	return fmt.Sprintf(", %d extraneous file(s) deleted", len(stats.Deleted))
}
