package aptdata

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed Debian package version: [epoch:]upstream[-revision].
//
// Its ordering is dpkg's, reimplemented here rather than shelled out to
// dpkg --compare-versions, so version selection works on hosts with no Debian
// tooling installed (and without a process per comparison).
type Version struct {
	Epoch    int
	Upstream string
	Revision string
}

// ParseVersion splits a version string into its three parts. It is lenient in
// the same places dpkg is: a missing epoch is 0 and a missing revision is empty.
// A syntactically invalid version is an error, since indexing one would produce
// a repository apt cannot order.
func ParseVersion(s string) (Version, error) {
	var v Version
	raw := strings.TrimSpace(s)
	if raw == "" {
		return v, fmt.Errorf("empty version")
	}

	// Epoch: digits before the first colon. A colon with a non-numeric prefix
	// is part of the upstream version (which may legally contain colons once an
	// epoch is present), so only a fully numeric prefix counts.
	if i := strings.Index(raw, ":"); i >= 0 {
		if n, err := strconv.Atoi(raw[:i]); err == nil && i > 0 {
			if n < 0 {
				return v, fmt.Errorf("negative epoch in %q", s)
			}
			v.Epoch = n
			raw = raw[i+1:]
		}
	}

	// Revision: everything after the *last* hyphen. A version with no hyphen is
	// a Debian-native package and has no revision.
	if i := strings.LastIndex(raw, "-"); i >= 0 {
		v.Upstream, v.Revision = raw[:i], raw[i+1:]
	} else {
		v.Upstream = raw
	}

	if v.Upstream == "" {
		return v, fmt.Errorf("version %q has an empty upstream part", s)
	}
	if !isDigit(v.Upstream[0]) {
		return v, fmt.Errorf("version %q: upstream version must start with a digit", s)
	}
	for _, c := range []byte(v.Upstream) {
		if !isAlnum(c) && !strings.ContainsRune(".+-:~", rune(c)) {
			return v, fmt.Errorf("version %q: invalid character %q in upstream version", s, c)
		}
	}
	for _, c := range []byte(v.Revision) {
		if !isAlnum(c) && !strings.ContainsRune(".+~", rune(c)) {
			return v, fmt.Errorf("version %q: invalid character %q in revision", s, c)
		}
	}
	return v, nil
}

// MustParseVersion is ParseVersion for versions already known to be valid
// (those read back out of a package we indexed). An invalid version yields a
// zero-epoch version holding the raw string, so a malformed index degrades to
// string ordering rather than panicking mid-publish.
func MustParseVersion(s string) Version {
	if v, err := ParseVersion(s); err == nil {
		return v
	}
	return Version{Upstream: s}
}

// String renders the canonical version string. The epoch is omitted when zero,
// matching how dpkg and apt display versions.
func (v Version) String() string {
	var b strings.Builder
	if v.Epoch != 0 {
		fmt.Fprintf(&b, "%d:", v.Epoch)
	}
	b.WriteString(v.Upstream)
	if v.Revision != "" {
		b.WriteString("-")
		b.WriteString(v.Revision)
	}
	return b.String()
}

// CompareVersions orders two version strings, returning -1, 0 or 1. It is the
// exported entry point for callers holding unparsed strings.
func CompareVersions(a, b string) int {
	return MustParseVersion(a).Compare(MustParseVersion(b))
}

// Compare orders v against o, returning -1 if v sorts first, 0 if they are
// equal, and 1 if v sorts last. This is dpkg's algorithm: epochs numerically,
// then upstream, then revision, each with verrevcmp.
func (v Version) Compare(o Version) int {
	if v.Epoch != o.Epoch {
		if v.Epoch < o.Epoch {
			return -1
		}
		return 1
	}
	if c := verrevcmp(v.Upstream, o.Upstream); c != 0 {
		return c
	}
	return verrevcmp(v.Revision, o.Revision)
}

// verrevcmp is dpkg's version-component comparison. It alternates between runs
// of non-digits (compared character by character under versionOrder) and runs
// of digits (compared numerically, ignoring leading zeros).
func verrevcmp(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		firstDiff := 0

		// Non-digit run: compare under the order that puts '~' before the end
		// of the string, letters before it, and everything else after.
		for (i < len(a) && !isDigit(a[i])) || (j < len(b) && !isDigit(b[j])) {
			ac, bc := 0, 0
			if i < len(a) {
				ac = versionOrder(a[i])
			}
			if j < len(b) {
				bc = versionOrder(b[j])
			}
			if ac != bc {
				return sign(ac - bc)
			}
			i++
			j++
		}

		// Leading zeros are not significant.
		for i < len(a) && a[i] == '0' {
			i++
		}
		for j < len(b) && b[j] == '0' {
			j++
		}

		// Digit run: the longer run is the larger number, and among runs of
		// equal length the first differing digit decides.
		for i < len(a) && isDigit(a[i]) && j < len(b) && isDigit(b[j]) {
			if firstDiff == 0 {
				firstDiff = int(a[i]) - int(b[j])
			}
			i++
			j++
		}
		if i < len(a) && isDigit(a[i]) {
			return 1
		}
		if j < len(b) && isDigit(b[j]) {
			return -1
		}
		if firstDiff != 0 {
			return sign(firstDiff)
		}
	}
	return 0
}

// versionOrder is dpkg's per-character weight. A tilde sorts before anything,
// including the end of the string, which is what makes 1.0~rc1 precede 1.0.
// Letters keep their ASCII value and every other character is pushed above
// them, so 1.0 precedes 1.0-1 but 1.0a precedes 1.0+.
func versionOrder(c byte) int {
	switch {
	case isDigit(c):
		return 0
	case isAlpha(c):
		return int(c)
	case c == '~':
		return -1
	default:
		return int(c) + 256
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isAlnum(c byte) bool { return isDigit(c) || isAlpha(c) }

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
