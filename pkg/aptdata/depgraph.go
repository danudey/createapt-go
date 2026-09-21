package aptdata

// This file implements a small dependency solver over a repository's binary
// package set. It answers two questions used when verifying or pruning a
// repository:
//
//   - CheckDependencies: which Depends/Pre-Depends of the repository's packages
//     are unmet, considering only intra-repository dependencies. A dependency on
//     a package no package in the repository provides at all — libc6, dpkg,
//     anything from the base distribution — is an external dependency and out of
//     scope, because the repository is not expected to carry it.
//   - RemovalBreakages: which packages cannot be removed from a set without
//     leaving some surviving package's dependency unsatisfied — e.g. package B
//     depends on A (= 1.0) specifically, so A 1.0 must not be pruned even though
//     a newer A 1.2 exists.
//
// Source packages have no runtime dependencies and their Build-Depends are
// resolved against a build chroot, not against the repository, so they take no
// part in either answer.

import (
	"fmt"
	"strings"
)

// provider records that a package offers a capability at a version. A package's
// own name is provided at its own version; a Provides entry is provided at the
// version it names, or unversioned.
type provider struct {
	pkg     *Package
	version string
}

// providerIndex maps capability names to the packages offering them.
type providerIndex struct {
	caps map[string][]provider
}

func buildProviderIndex(pkgs []*Package) *providerIndex {
	pi := &providerIndex{caps: map[string][]provider{}}
	for _, p := range pkgs {
		for name, version := range p.ProvidedCapabilities() {
			pi.caps[name] = append(pi.caps[name], provider{pkg: p, version: version})
		}
	}
	return pi
}

// satisfy returns the first package that satisfies r, or nil.
func (pi *providerIndex) satisfy(r Relation) *Package {
	for _, pr := range pi.caps[r.Name] {
		if r.Satisfies(pr.version) {
			return pr.pkg
		}
	}
	return nil
}

// satisfyGroup returns the first package satisfying any alternative in the
// group, or nil when none does.
func (pi *providerIndex) satisfyGroup(g Alternatives, arch string) *Package {
	for _, r := range g {
		if !r.AppliesTo(arch) {
			continue
		}
		if p := pi.satisfy(r); p != nil {
			return p
		}
	}
	return nil
}

// groupKnown reports whether the repository provides any of the group's
// capability names at all (ignoring versions). That is what distinguishes an
// intra-repository dependency, whose version constraint the repository is
// responsible for, from an external one it merely names.
func (pi *providerIndex) groupKnown(g Alternatives, arch string) bool {
	for _, r := range g {
		if !r.AppliesTo(arch) {
			continue
		}
		if len(pi.caps[r.Name]) > 0 {
			return true
		}
	}
	return false
}

// hardRelations returns the Depends and Pre-Depends groups of a package that
// apply to its architecture, skipping anything unexpanded (a stanza still
// carrying a ${shlibs:Depends} substitution variable was never built properly,
// and guessing at it would produce noise rather than a finding).
func hardRelations(p *Package) []Alternatives {
	var out []Alternatives
	arch := p.Arch()
	for _, field := range HardDependencyFields {
		for _, g := range p.Relations(field) {
			if len(g) == 0 || strings.Contains(g.String(), "${") {
				continue
			}
			if !anyApplies(g, arch) {
				continue
			}
			out = append(out, g)
		}
	}
	return out
}

func anyApplies(g Alternatives, arch string) bool {
	for _, r := range g {
		if r.AppliesTo(arch) {
			return true
		}
	}
	return false
}

// DependencyProblem is a dependency of a package in the repository that the
// repository's own packages could satisfy by name — some package provides the
// capability — but no available version satisfies the version constraint. A
// dependency on a name no package in the repository provides is treated as
// external (libc6, dpkg) and is not reported.
type DependencyProblem struct {
	Package  *Package
	Requires Alternatives
}

func (p DependencyProblem) String() string {
	return fmt.Sprintf("%s depends on %q, which no available version provides",
		p.Package.ID3(), p.Requires.String())
}

// CheckDependencies reports every intra-repository dependency that no package
// in pkgs satisfies. It is used to verify a repository and to detect a prune or
// removal that has broken the dependency graph. External dependencies are out
// of scope; see DependencyProblem.
//
// An arch:all package's dependencies are checked against the whole package set,
// since it is installed on every architecture; an architecture-specific package
// is checked against the packages available for its own architecture plus the
// arch:all ones, which is what apt would see.
func CheckDependencies(pkgs []*Package) []DependencyProblem {
	var problems []DependencyProblem
	for arch, set := range perArchSets(pkgs) {
		pi := buildProviderIndex(set)
		for _, p := range set {
			// Every package is checked exactly once, in the set matching its
			// own architecture. The other packages in that set are present as
			// potential providers, not as subjects: an arch:all package appears
			// in every architecture's set, and an architecture-specific one
			// appears in the "all" set, so checking them here too would report
			// the same problem once per architecture.
			if p.Arch() != arch {
				continue
			}
			for _, g := range hardRelations(p) {
				if pi.satisfyGroup(g, p.Arch()) != nil {
					continue
				}
				if !pi.groupKnown(g, p.Arch()) {
					continue // external dependency, not the repository's concern
				}
				problems = append(problems, DependencyProblem{Package: p, Requires: g})
			}
		}
	}
	sortProblems(problems)
	return problems
}

// perArchSets groups packages into the sets apt would resolve against: one per
// real architecture, each including the arch:all packages, plus an "all"-only
// set so an arch:all package's own dependencies are checked exactly once.
func perArchSets(pkgs []*Package) map[string][]*Package {
	var allArch []*Package
	byArch := map[string][]*Package{}
	for _, p := range pkgs {
		if p.Arch() == ArchAll {
			allArch = append(allArch, p)
			continue
		}
		byArch[p.Arch()] = append(byArch[p.Arch()], p)
	}
	for arch := range byArch {
		byArch[arch] = append(byArch[arch], allArch...)
	}
	if len(allArch) > 0 {
		// Resolve an arch:all package against every package in the repository:
		// it installs anywhere, so any architecture's providers may serve it.
		byArch[ArchAll] = pkgs
	}
	return byArch
}

func sortProblems(ps []DependencyProblem) {
	// Deterministic output: by dependent, then by the requirement's text.
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && problemLess(ps[j], ps[j-1]); j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}

func problemLess(a, b DependencyProblem) bool {
	if a.Package.ID3() != b.Package.ID3() {
		return a.Package.ID3() < b.Package.ID3()
	}
	return a.Requires.String() < b.Requires.String()
}

// Breakage records that a package (Dependent) has a dependency (Requires) that
// only Provider satisfies among the survivors of a proposed removal — so
// removing Provider would leave Dependent's dependency unmet.
type Breakage struct {
	Provider  *Package // the version that uniquely satisfies the dependency
	Dependent *Package // the package that depends on Provider
	Requires  Alternatives
}

func (b Breakage) String() string {
	return fmt.Sprintf("%s is required by %s (%q)",
		b.Provider.ID3(), b.Dependent.ID3(), b.Requires.String())
}

// RemovalBreakages reports which of the removing packages cannot be dropped
// from the set `all` without breaking a package that survives the removal. For
// each surviving package whose dependency would become unsatisfied once the
// removing set is gone, it returns the removed package(s) that were satisfying
// it.
//
// A removed package can appear in several breakages (needed by more than one
// dependent), and a dependent can appear several times (its dependency was met
// by more than one removed provider). `all` must include the removing packages.
func RemovalBreakages(all, removing []*Package) []Breakage {
	if len(removing) == 0 {
		return nil
	}
	removeSet := make(map[string]bool, len(removing))
	for _, p := range removing {
		removeSet[p.ID()] = true
	}
	keep := make([]*Package, 0, len(all))
	for _, p := range all {
		if !removeSet[p.ID()] {
			keep = append(keep, p)
		}
	}
	keepIdx := buildProviderIndex(keep)
	removingIdx := buildProviderIndex(removing)

	var out []Breakage
	for _, q := range keep {
		for _, g := range hardRelations(q) {
			// Still satisfied by a survivor? Then the removal does not break q.
			if keepIdx.satisfyGroup(g, q.Arch()) != nil {
				continue
			}
			// Otherwise attribute the break to every removed package that was
			// satisfying it (an external dependency matches none).
			for _, r := range g {
				if !r.AppliesTo(q.Arch()) {
					continue
				}
				for _, pr := range removingIdx.caps[r.Name] {
					if r.Satisfies(pr.version) {
						out = append(out, Breakage{Provider: pr.pkg, Dependent: q, Requires: g})
					}
				}
			}
		}
	}
	return out
}
