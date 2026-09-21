// Package repocheck validates apt (Debian/Ubuntu) repositories. It reads a
// suite's Release file, checks every index it covers against the size and
// checksums recorded there, parses those indexes, and confirms the packages
// they reference actually exist with the size and checksum claimed.
//
// This catches repositories where a published package file has diverged from
// what the indexes describe — for example when one .deb is published over
// another without regenerating the metadata — which end users see as:
//
//	Failed to fetch .../pool/main/h/hello_2.10-3_amd64.deb
//	  Hash Sum mismatch
//
// Unlike a purely HTTP-based checker, repocheck runs against any repository the
// backend package can reach — a local directory, an SSH/SFTP host, an S3 or GCS
// bucket, or an HTTP(S) server — so the same validation covers a repository at
// rest as well as one published live.
package repocheck

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/backend"
	"github.com/danudey/createapt-go/pkg/repoconfig"
)

const userAgent = "createapt-go/check (+https://github.com/danudey/createapt-go)"

// Options describes a full validation run for Run.
type Options struct {
	// Input is a repository location (local path or file://, sftp://, s3://,
	// gs:// or http(s):// URL), or a path/URL to a sources file (ending in
	// ".list" or ".sources").
	Input string
	// Suites restricts which suites are checked when Input is a repository
	// location. Empty means the one recorded in the repository's config, or
	// "stable".
	Suites []string
	// Components restricts which components are checked; empty means every
	// component the Release lists.
	Components []string
	// Arches restricts which package architectures are checked; empty means
	// every architecture present in the indexes.
	Arches []string
	// Packages restricts which package names are checked; empty means all.
	Packages []string
	// Level and LatestOnly control the depth and breadth of the checks.
	Level      Level
	LatestOnly bool
	// Concurrency bounds the number of packages checked in parallel.
	Concurrency int
	// Timeout bounds each individual backend operation. Zero means no timeout.
	Timeout time.Duration

	// Deb is the archive verifier used at the fetch level. Build it with
	// DetectDebTool.
	Deb DebTool

	// Logf, when non-nil, receives a line for every individual check.
	Logf func(format string, args ...any)
	// OnTargetStart, when non-nil, is called once per concrete suite before its
	// checks begin (useful for printing a per-target header).
	OnTargetStart func(t Target)
}

// Target identifies a concrete suite that was checked.
type Target struct {
	Label      string
	BaseURL    string
	Suite      string
	Components []string
}

// Run executes the validation described by opts. It returns the per-artifact
// results and any non-fatal configuration warnings (e.g. skipped entries). A
// returned error indicates the run could not be performed at all (bad input, or
// no repositories to check); per-artifact failures are reported via the
// results' Status, not the error.
func Run(ctx context.Context, opts Options) (results []Result, warnings []string, err error) {
	targets, warnings, err := resolveTargets(ctx, opts, fetchOverHTTP(ctx, opts.Timeout))
	if err != nil {
		return nil, warnings, err
	}
	if len(targets) == 0 {
		return nil, warnings, fmt.Errorf("no repositories to check")
	}

	cfg := Config{
		Level:       opts.Level,
		LatestOnly:  opts.LatestOnly,
		Arches:      opts.Arches,
		Packages:    opts.Packages,
		Concurrency: opts.Concurrency,
		Timeout:     opts.Timeout,
	}
	ck := newChecker(cfg, opts.Deb, opts.Logf)

	for _, t := range targets {
		if opts.OnTargetStart != nil {
			opts.OnTargetStart(t)
		}
		// Each target carries its own component restriction, because a sources
		// file can name different components per entry.
		ck.cfg.Components = t.Components

		be, err := backend.Open(ctx, t.BaseURL)
		if err != nil {
			ck.add(Result{
				Target: t.Label, Kind: "repository", Loc: t.BaseURL, Status: StatusFail,
				Detail: "cannot open repository: " + err.Error(),
			})
			continue
		}
		ck.checkTarget(ctx, be, t.Label, t.Suite)
		if c, ok := be.(backend.Closer); ok {
			_ = c.Close()
		}
	}

	return ck.Results(), warnings, nil
}

// resolveTargets turns the run's input into the concrete suites to check.
func resolveTargets(ctx context.Context, opts Options, fetch func(string) ([]byte, error)) ([]Target, []string, error) {
	if IsSourcesFile(opts.Input) {
		entries, warnings, err := LoadSources(opts.Input, fetch)
		if err != nil {
			return nil, warnings, err
		}
		var targets []Target
		for _, e := range entries {
			// A deb-src entry names the same indexes a deb entry does, plus the
			// source one; both are covered by checking the suite, so listing
			// the suite twice would only double the work.
			if e.Source && hasBinaryEntryFor(entries, e) {
				continue
			}
			if len(opts.Suites) > 0 && !contains(opts.Suites, e.Suite) {
				continue
			}
			targets = append(targets, Target{
				Label:      e.Label(),
				BaseURL:    strings.TrimRight(e.URI, "/"),
				Suite:      e.Suite,
				Components: mergeComponents(e.Components, opts.Components),
			})
		}
		if len(targets) == 0 {
			return nil, warnings, fmt.Errorf("no entries in %s match the requested suites", opts.Input)
		}
		return targets, warnings, nil
	}

	suites := opts.Suites
	if len(suites) == 0 {
		// With no suite named, check the one the repository says it publishes.
		// Defaulting to "stable" instead would report a repository built for
		// any other suite as entirely missing, which is both wrong and the most
		// confusing possible answer.
		suites = []string{recordedSuite(ctx, opts.Input)}
	}
	var targets []Target
	for _, suite := range suites {
		if suite == "" {
			suite = defaultSuite
		}
		if err := aptdata.ValidSuite(suite); err != nil {
			return nil, nil, err
		}
		targets = append(targets, Target{
			Label:      fmt.Sprintf("%s %s", opts.Input, suite),
			BaseURL:    opts.Input,
			Suite:      suite,
			Components: opts.Components,
		})
	}
	return targets, nil, nil
}

// defaultSuite mirrors repo.DefaultSuite. It is repeated rather than imported
// so repocheck stays independent of the mutation layer.
const defaultSuite = "stable"

// recordedSuite reads the suite a repository's config file records, returning
// "" when the repository has no config, cannot be opened, or records no suite.
// Every one of those cases falls back to the default, so a failure here only
// ever costs the caller a clearer error later.
func recordedSuite(ctx context.Context, location string) string {
	be, err := backend.Open(ctx, location)
	if err != nil {
		return ""
	}
	if c, ok := be.(backend.Closer); ok {
		defer func() { _ = c.Close() }()
	}
	cfg, err := repoconfig.Load(ctx, be)
	if err != nil || cfg == nil {
		return ""
	}
	return cfg.Suite
}

// hasBinaryEntryFor reports whether a deb entry already covers the same suite
// as the given deb-src entry.
func hasBinaryEntryFor(entries []SourceEntry, src SourceEntry) bool {
	for _, e := range entries {
		if !e.Source && e.URI == src.URI && e.Suite == src.Suite {
			return true
		}
	}
	return false
}

// mergeComponents narrows an entry's components by the run's --component
// restriction, keeping the entry's own list when no restriction was given.
func mergeComponents(entry, requested []string) []string {
	if len(requested) == 0 {
		return entry
	}
	if len(entry) == 0 {
		return requested
	}
	var out []string
	for _, c := range entry {
		if contains(requested, c) {
			out = append(out, c)
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// fetchOverHTTP returns a function that downloads a sources file over HTTP(S).
func fetchOverHTTP(ctx context.Context, timeout time.Duration) func(string) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	return func(rawURL string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %s", resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
}
