package repocheck

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danudey/createapt-go/pkg/aptdata"
	"github.com/danudey/createapt-go/pkg/backend"
	"github.com/danudey/createapt-go/pkg/debmeta"
)

// Level selects how deep the checks go. Each level is cumulative.
type Level int

// The available check levels, in increasing order of cost.
const (
	LevelMetadata Level = iota // Release + index files present and valid
	LevelHead                  // + packages exist with the correct size
	LevelFetch                 // + packages downloaded, checksums verified, archive parsed
)

func (l Level) String() string {
	switch l {
	case LevelMetadata:
		return "metadata"
	case LevelHead:
		return "head"
	case LevelFetch:
		return "fetch"
	default:
		return "unknown"
	}
}

// ParseLevel parses a Level from its string name.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "metadata", "meta":
		return LevelMetadata, nil
	case "head", "exists":
		return LevelHead, nil
	case "fetch", "full", "download":
		return LevelFetch, nil
	default:
		return 0, fmt.Errorf("invalid level %q (want metadata|head|fetch)", s)
	}
}

// Status is the outcome of a single check.
type Status string

// The outcomes a single check can report. Only StatusFail means the repository
// is broken; StatusWarn and StatusSkip leave the run successful.
const (
	StatusOK   Status = "OK"
	StatusFail Status = "FAIL"
	StatusWarn Status = "WARN"
	StatusSkip Status = "SKIP"
)

// The Result.Kind values reported for the repository's own metadata.
const (
	kindRelease = "Release"
	kindIndex   = "index"
)

// Result is one validated artifact.
type Result struct {
	Target string // the label of the repository the artifact belongs to
	Kind   string // "Release", "index", or a package identifier
	Loc    string // backend-relative path of the artifact
	Status Status
	Detail string
}

// Config holds the resolved validation configuration.
type Config struct {
	Level       Level
	LatestOnly  bool
	Arches      []string // empty means "all architectures in the metadata"
	Components  []string // empty means "all components the Release lists"
	Packages    []string // empty means "all packages"
	Concurrency int
	// Timeout bounds each individual backend operation. Zero means no timeout.
	Timeout time.Duration
}

// checker runs validations against targets and accumulates results.
type checker struct {
	cfg Config
	deb DebTool

	mu      sync.Mutex
	results []Result
	logf    func(format string, args ...any)
}

func newChecker(cfg Config, deb DebTool, logf func(string, ...any)) *checker {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &checker{cfg: cfg, deb: deb, logf: logf}
}

// Results returns a copy of the accumulated results.
func (ck *checker) Results() []Result {
	ck.mu.Lock()
	defer ck.mu.Unlock()
	out := make([]Result, len(ck.results))
	copy(out, ck.results)
	return out
}

func (ck *checker) add(r Result) {
	ck.mu.Lock()
	ck.results = append(ck.results, r)
	ck.mu.Unlock()
	symbol := map[Status]string{StatusOK: "  ok ", StatusFail: "FAIL ", StatusWarn: "warn ", StatusSkip: "skip "}[r.Status]
	ck.logf("[%s] %s: %s %s", symbol, r.Target, r.Kind, r.Detail)
}

// opCtx derives a per-operation context honoring the configured timeout.
func (ck *checker) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ck.cfg.Timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, ck.cfg.Timeout)
}

// checkTarget validates one suite of one repository.
func (ck *checker) checkTarget(ctx context.Context, be backend.Backend, label, suite string) {
	rel, ok := ck.checkRelease(ctx, be, label, suite)
	if !ok {
		return
	}

	indexes := ck.checkIndexes(ctx, be, label, suite, rel)
	if ck.cfg.Level == LevelMetadata {
		return
	}

	entries := ck.selectEntries(indexes)
	if len(entries) == 0 {
		ck.add(Result{
			Target: label, Kind: kindIndex, Loc: aptdata.SuiteDir(suite), Status: StatusWarn,
			Detail: "no packages selected to check",
		})
		return
	}
	ck.checkEntries(ctx, be, label, entries)
}

// checkRelease reads and parses the suite's Release file. It prefers InRelease
// because that is the document apt trusts, and falls back to the plain Release.
func (ck *checker) checkRelease(ctx context.Context, be backend.Backend, label, suite string) (*aptdata.Release, bool) {
	for _, spec := range []struct {
		path      string
		clearsign bool
	}{
		{aptdata.InReleasePath(suite), true},
		{aptdata.ReleasePath(suite), false},
	} {
		opCtx, cancel := ck.opCtx(ctx)
		data, err := readAll(opCtx, be, spec.path)
		cancel()
		if errors.Is(err, backend.ErrNotExist) {
			continue
		}
		if err != nil {
			ck.add(Result{
				Target: label, Kind: kindRelease, Loc: spec.path, Status: StatusFail,
				Detail: "cannot read: " + err.Error(),
			})
			return nil, false
		}
		if spec.clearsign {
			data = debmeta.StripClearsign(data)
		}
		rel, err := aptdata.ParseRelease(data)
		if err != nil {
			ck.add(Result{
				Target: label, Kind: kindRelease, Loc: spec.path, Status: StatusFail,
				Detail: "cannot parse: " + err.Error(),
			})
			return nil, false
		}
		ck.add(Result{
			Target: label, Kind: kindRelease, Loc: spec.path, Status: StatusOK,
			Detail: fmt.Sprintf("parsed, covering %d index file(s)", len(rel.Files)),
		})
		ck.checkExpiry(rel, label, spec.path)
		return rel, true
	}

	ck.add(Result{
		Target: label, Kind: kindRelease, Loc: aptdata.ReleasePath(suite), Status: StatusFail,
		Detail: "neither InRelease nor Release is present",
	})
	return nil, false
}

// checkExpiry warns about a Release that has passed its Valid-Until, which apt
// refuses outright even though every file it names may be intact.
func (ck *checker) checkExpiry(rel *aptdata.Release, label, loc string) {
	until := rel.ValidUntil()
	if until.IsZero() {
		return
	}
	if time.Now().After(until) {
		ck.add(Result{
			Target: label, Kind: kindRelease, Loc: loc, Status: StatusFail,
			Detail: fmt.Sprintf("expired: Valid-Until was %s; apt will refuse this repository", until.Format(aptdata.DateLayout)),
		})
	}
}

// indexContents is the parsed content of one package index.
type indexContents struct {
	packages []*aptdata.Package
	sources  []*aptdata.Source
}

// checkIndexes validates every index the Release covers and returns the parsed
// package sets. An index is validated by reading it back and comparing its size
// and every checksum the Release records, then decompressing and parsing it —
// which together prove the Release describes a document apt can actually use.
func (ck *checker) checkIndexes(ctx context.Context, be backend.Backend, label, suite string, rel *aptdata.Release) indexContents {
	var out indexContents
	suiteDir := aptdata.SuiteDir(suite)

	// Each (component, architecture) index is published in several compressed
	// forms of the same document. All of them are integrity-checked, but only
	// one is parsed, because they decode to the same stanzas.
	parsed := map[string]bool{}

	for _, f := range sortedFiles(rel.Files) {
		full := path.Join(suiteDir, f.Path)
		component, arch, isPackageIndex := splitIndexPath(f.Path)
		if !ck.wantsComponent(component) {
			continue
		}

		opCtx, cancel := ck.opCtx(ctx)
		raw, err := readAll(opCtx, be, full)
		cancel()
		if err != nil {
			ck.add(Result{
				Target: label, Kind: kindIndex, Loc: full, Status: StatusFail,
				Detail: "cannot read: " + err.Error(),
			})
			continue
		}
		if detail, ok := checkAgainstRelease(f, raw); !ok {
			ck.add(Result{Target: label, Kind: kindIndex, Loc: full, Status: StatusFail, Detail: detail})
			continue
		}

		body, err := aptdata.Decompress(f.Path, raw)
		if err != nil {
			ck.add(Result{
				Target: label, Kind: kindIndex, Loc: full, Status: StatusFail,
				Detail: "cannot decompress: " + err.Error(),
			})
			continue
		}

		if !isPackageIndex {
			ck.add(Result{
				Target: label, Kind: kindIndex, Loc: full, Status: StatusOK,
				Detail: fmt.Sprintf("%d bytes, checksums match", f.Size),
			})
			continue
		}

		key := component + "/" + arch
		if parsed[key] {
			ck.add(Result{
				Target: label, Kind: kindIndex, Loc: full, Status: StatusOK,
				Detail: fmt.Sprintf("%d bytes, checksums match", f.Size),
			})
			continue
		}

		count, err := ck.ingest(&out, body, component, arch)
		if err != nil {
			ck.add(Result{
				Target: label, Kind: kindIndex, Loc: full, Status: StatusFail,
				Detail: "cannot parse: " + err.Error(),
			})
			continue
		}
		parsed[key] = true
		ck.add(Result{
			Target: label, Kind: kindIndex, Loc: full, Status: StatusOK,
			Detail: fmt.Sprintf("%d bytes, checksums match, %d stanza(s)", f.Size, count),
		})
	}
	return out
}

// ingest parses a package index into the accumulating contents.
func (ck *checker) ingest(out *indexContents, body []byte, component, arch string) (int, error) {
	if arch == aptdata.ArchSource {
		srcs, err := aptdata.ParseSources(body, component)
		if err != nil {
			return 0, err
		}
		out.sources = append(out.sources, srcs...)
		return len(srcs), nil
	}
	pkgs, err := aptdata.ParsePackages(body, component)
	if err != nil {
		return 0, err
	}
	out.packages = append(out.packages, pkgs...)
	return len(pkgs), nil
}

// checkAgainstRelease compares a file's bytes against the size and every
// checksum the Release records for it.
func checkAgainstRelease(f aptdata.IndexFile, raw []byte) (string, bool) {
	if f.Size > 0 && int64(len(raw)) != f.Size {
		return fmt.Sprintf("size %d does not match the %d the Release records", len(raw), f.Size), false
	}
	algos := f.Hashes.Algorithms()
	if len(algos) == 0 {
		return "the Release records no checksum for this file", false
	}
	got := aptdata.HashBytes(raw, algos)
	for _, algo := range algos {
		if !strings.EqualFold(got[algo], f.Hashes[algo]) {
			return fmt.Sprintf("%s is %s but the Release records %s", algo, got[algo], f.Hashes[algo]), false
		}
	}
	return "", true
}

// splitIndexPath recognizes a package index path within a suite, returning its
// component and architecture. Anything else the Release lists (a per-component
// Release file, a Contents or Translation file) reports false.
func splitIndexPath(p string) (component, arch string, ok bool) {
	segments := strings.Split(p, "/")
	if len(segments) < 3 {
		return "", "", false
	}
	base := segments[len(segments)-1]
	dir := segments[len(segments)-2]
	component = strings.Join(segments[:len(segments)-2], "/")
	if component == "" {
		return "", "", false
	}
	base = stripCompressionExt(base)
	switch {
	case dir == aptdata.ArchSource && base == "Sources":
		return component, aptdata.ArchSource, true
	case strings.HasPrefix(dir, "binary-") && base == "Packages":
		return component, strings.TrimPrefix(dir, "binary-"), true
	default:
		return "", "", false
	}
}

func stripCompressionExt(base string) string {
	switch strings.ToLower(path.Ext(base)) {
	case ".gz", ".xz", ".zst", ".zstd", ".bz2", ".lzma":
		return strings.TrimSuffix(base, path.Ext(base))
	default:
		return base
	}
}

// wantsComponent reports whether a component is in scope for this run. A path
// outside any component (there is none to name) is always in scope.
func (ck *checker) wantsComponent(component string) bool {
	if component == "" || len(ck.cfg.Components) == 0 {
		return true
	}
	for _, c := range ck.cfg.Components {
		if c == component {
			return true
		}
	}
	return false
}

// selectEntries narrows the parsed indexes to the entries this run checks,
// applying the architecture, package-name and latest-version filters.
//
// A package is de-duplicated by checksum, because an arch:all package appears
// in every architecture's index and checking it once per architecture would
// multiply the work for no extra assurance.
func (ck *checker) selectEntries(contents indexContents) []aptdata.Entry {
	byID := map[string]aptdata.Entry{}
	var order []aptdata.Entry
	addUnique := func(e aptdata.Entry) {
		if _, seen := byID[e.ID()]; seen {
			return
		}
		byID[e.ID()] = e
		order = append(order, e)
	}

	for _, p := range contents.packages {
		if ck.wantsArch(p.Arch()) && ck.wantsName(p.Name()) {
			addUnique(p)
		}
	}
	for _, s := range contents.sources {
		if ck.wantsArch(aptdata.ArchSource) && ck.wantsName(s.Name()) {
			addUnique(s)
		}
	}

	if ck.cfg.LatestOnly {
		best := map[string]aptdata.Entry{}
		for _, e := range order {
			if cur, seen := best[e.Key()]; !seen || cur.ParsedVersion().Compare(e.ParsedVersion()) < 0 {
				best[e.Key()] = e
			}
		}
		kept := order[:0]
		for _, e := range order {
			if best[e.Key()] == e {
				kept = append(kept, e)
			}
		}
		order = kept
	}

	sort.Slice(order, func(i, j int) bool { return order[i].ID3() < order[j].ID3() })
	return order
}

func (ck *checker) wantsArch(arch string) bool {
	if len(ck.cfg.Arches) == 0 {
		return true
	}
	for _, a := range ck.cfg.Arches {
		if a == "any" || a == arch {
			return true
		}
		// An arch:all package installs on every architecture, so it is in scope
		// whenever any real architecture is.
		if arch == aptdata.ArchAll && a != aptdata.ArchSource {
			return true
		}
	}
	return false
}

func (ck *checker) wantsName(name string) bool {
	if len(ck.cfg.Packages) == 0 {
		return true
	}
	for _, n := range ck.cfg.Packages {
		if n == name {
			return true
		}
	}
	return false
}

// checkEntries validates the selected entries in parallel.
func (ck *checker) checkEntries(ctx context.Context, be backend.Backend, label string, entries []aptdata.Entry) {
	conc := ck.cfg.Concurrency
	if conc < 1 {
		conc = 1
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, conc)
	for _, e := range entries {
		wg.Add(1)
		sem <- struct{}{}
		go func(e aptdata.Entry) {
			defer wg.Done()
			defer func() { <-sem }()
			ck.checkEntry(ctx, be, label, e)
		}(e)
	}
	wg.Wait()
}

// checkEntry validates every file an entry owns.
func (ck *checker) checkEntry(ctx context.Context, be backend.Backend, label string, e aptdata.Entry) {
	for _, f := range entryFiles(e) {
		if ck.cfg.Level >= LevelFetch {
			ck.fetchAndVerify(ctx, be, label, e, f)
			continue
		}
		ck.headCheck(ctx, be, label, e, f)
	}
}

// checkFile is one published file with what the indexes say about it.
type checkFile struct {
	loc    string
	size   int64
	sha256 string
	isDeb  bool
}

func entryFiles(e aptdata.Entry) []checkFile {
	switch t := e.(type) {
	case *aptdata.Package:
		return []checkFile{{loc: t.Location(), size: t.Size(), sha256: t.ID(), isDeb: true}}
	case *aptdata.Source:
		dir := t.Directory()
		files := t.SourceFiles()
		out := make([]checkFile, 0, len(files))
		for _, f := range files {
			out = append(out, checkFile{loc: f.Location(dir), size: f.Size, sha256: f.SHA256})
		}
		return out
	default:
		return nil
	}
}

// headCheck confirms a file exists with the size the index records, and its
// checksum too when the backend can supply one without transferring the file.
func (ck *checker) headCheck(ctx context.Context, be backend.Backend, label string, e aptdata.Entry, f checkFile) {
	opCtx, cancel := ck.opCtx(ctx)
	defer cancel()

	fi, err := be.Stat(opCtx, f.loc)
	if errors.Is(err, backend.ErrNotExist) {
		ck.add(Result{
			Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusFail,
			Detail: "the file the index references is missing",
		})
		return
	}
	if err != nil {
		ck.add(Result{
			Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusFail,
			Detail: "cannot stat: " + err.Error(),
		})
		return
	}
	if f.size > 0 && fi.Size != f.size {
		ck.add(Result{
			Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusFail,
			Detail: fmt.Sprintf("size %d does not match the %d the index records", fi.Size, f.size),
		})
		return
	}

	if hasher, ok := be.(backend.RemoteHasher); ok && f.sha256 != "" {
		sum, ok, err := hasher.Hash(opCtx, f.loc, backend.AlgoSHA256)
		if err != nil && !errors.Is(err, backend.ErrNotExist) {
			ck.add(Result{
				Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusWarn,
				Detail: "checksum could not be read: " + err.Error(),
			})
			return
		}
		if ok {
			if !strings.EqualFold(sum, f.sha256) {
				ck.add(Result{
					Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusFail,
					Detail: fmt.Sprintf("checksum %s does not match the %s the index records", sum, f.sha256),
				})
				return
			}
			ck.add(Result{
				Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusOK,
				Detail: fmt.Sprintf("present, %d bytes, checksum matches", fi.Size),
			})
			return
		}
	}
	ck.add(Result{
		Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusOK,
		Detail: fmt.Sprintf("present, %d bytes (checksum not verified at this level)", fi.Size),
	})
}

// fetchAndVerify downloads a file and proves its size and checksum from its
// content, then — for a .deb, and when the tooling is available — confirms the
// archive itself parses.
func (ck *checker) fetchAndVerify(ctx context.Context, be backend.Backend, label string, e aptdata.Entry, f checkFile) {
	opCtx, cancel := ck.opCtx(ctx)
	defer cancel()

	local, size, sum, err := fetchToTempFile(opCtx, be, f.loc)
	if err != nil {
		ck.add(Result{
			Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusFail,
			Detail: "cannot download: " + err.Error(),
		})
		return
	}
	defer removeFile(local)

	if f.size > 0 && size != f.size {
		ck.add(Result{
			Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusFail,
			Detail: fmt.Sprintf("downloaded %d bytes, the index records %d", size, f.size),
		})
		return
	}
	if f.sha256 != "" && !strings.EqualFold(sum, f.sha256) {
		ck.add(Result{
			Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusFail,
			Detail: fmt.Sprintf("content checksum %s does not match the %s the index records", sum, f.sha256),
		})
		return
	}

	if f.isDeb {
		if err := ck.deb.Verify(local); err != nil {
			if errors.Is(err, errDebToolUnavailable) {
				ck.add(Result{
					Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusOK,
					Detail: fmt.Sprintf("%d bytes, checksum verified (archive not opened: %v)", size, err),
				})
				return
			}
			ck.add(Result{
				Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusFail,
				Detail: "the archive does not parse: " + err.Error(),
			})
			return
		}
		ck.add(Result{
			Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusOK,
			Detail: fmt.Sprintf("%d bytes, checksum verified, archive parses", size),
		})
		return
	}
	ck.add(Result{
		Target: label, Kind: e.ID3(), Loc: f.loc, Status: StatusOK,
		Detail: fmt.Sprintf("%d bytes, checksum verified", size),
	})
}

// sortedFiles returns the Release's index entries in a stable order.
func sortedFiles(files []aptdata.IndexFile) []aptdata.IndexFile {
	out := append([]aptdata.IndexFile(nil), files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Summarize counts results by status.
func Summarize(results []Result) (ok, fail, warn, skip int) {
	for _, r := range results {
		switch r.Status {
		case StatusOK:
			ok++
		case StatusFail:
			fail++
		case StatusWarn:
			warn++
		case StatusSkip:
			skip++
		}
	}
	return ok, fail, warn, skip
}
