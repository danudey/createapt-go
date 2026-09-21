package aptdata

import "testing"

// TestCompareVersions covers the cases dpkg's own test suite uses, plus the
// ones that most often trip up a reimplementation: the tilde ordering, letters
// sorting before punctuation, leading zeros, and epochs dominating everything.
func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		// Equality, including forms that differ only in how they are written.
		{"1.0", "1.0", 0},
		{"0:1.0", "1.0", 0},
		{"1.0-1", "1.0-1", 0},
		{"1.0", "1.00", 0},
		{"1.0-01", "1.0-1", 0},

		// Plain numeric ordering.
		{"1.0", "1.1", -1},
		{"1.9", "1.10", -1},
		{"2.0", "1.999", 1},

		// The tilde sorts before everything, including the empty string, which
		// is what makes a release candidate precede its release.
		{"1.0~rc1", "1.0", -1},
		{"1.0~rc1", "1.0~rc2", -1},
		{"1.0~~", "1.0~", -1},
		{"1.0~", "1.0", -1},

		// Letters sort before non-letters, so 1.0a precedes 1.0+b.
		{"1.0a", "1.0+", -1},
		{"1.0a", "1.0b", -1},

		// The revision is compared only after the upstream version.
		{"1.0-1", "1.0-2", -1},
		{"1.0-10", "1.0-9", 1},
		{"1.0", "1.0-1", -1},

		// An epoch dominates the rest of the version outright.
		{"1:1.0", "2.0", 1},
		{"1:1.0", "2:0.1", -1},
		{"0:1.0", "1.0-1", -1},

		// A revision containing a hyphen: only the last one splits.
		{"1.0-1-2", "1.0-1-3", -1},
	}
	for _, tc := range tests {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d; want %d", tc.a, tc.b, got, tc.want)
		}
		// Comparison must be antisymmetric.
		if got := CompareVersions(tc.b, tc.a); got != -tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d; want %d", tc.b, tc.a, got, -tc.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in       string
		epoch    int
		upstream string
		revision string
	}{
		{"1.0", 0, "1.0", ""},
		{"1.0-3", 0, "1.0", "3"},
		{"2:1.0-3", 2, "1.0", "3"},
		{"1:2.3.4~beta1-1ubuntu2", 1, "2.3.4~beta1", "1ubuntu2"},
		// With an epoch present the upstream version may contain a colon.
		{"1:1.2:3-4", 1, "1.2:3", "4"},
	}
	for _, tc := range tests {
		v, err := ParseVersion(tc.in)
		if err != nil {
			t.Errorf("ParseVersion(%q) errored: %v", tc.in, err)
			continue
		}
		if v.Epoch != tc.epoch || v.Upstream != tc.upstream || v.Revision != tc.revision {
			t.Errorf("ParseVersion(%q) = {%d %q %q}; want {%d %q %q}",
				tc.in, v.Epoch, v.Upstream, v.Revision, tc.epoch, tc.upstream, tc.revision)
		}
		if got := v.String(); got != tc.in {
			t.Errorf("ParseVersion(%q).String() = %q; want a round trip", tc.in, got)
		}
	}
}

func TestParseVersionRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"",
		"abc",      // upstream must start with a digit
		"1.0-a_b",  // underscore is not valid in a revision
		"1.0 beta", // whitespace is not valid
	} {
		if _, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) succeeded; want an error", in)
		}
	}
}
