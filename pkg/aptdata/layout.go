package aptdata

import (
	"fmt"
	"path"
	"strings"
)

// The fixed top-level directories of a Debian repository.
const (
	// DistsDir holds one subdirectory per suite, each with a Release file and
	// the per-component indexes it covers.
	DistsDir = "dists"
	// PoolDir holds the package files themselves, shared by every suite that
	// indexes them.
	PoolDir = "pool"
	// ByHashDir is the subdirectory of an index directory holding each index
	// under its own checksum, as Acquire-By-Hash describes.
	ByHashDir = "by-hash"
)

// Dist names one suite within a repository. A suite is the unit a Release file
// covers and that a client subscribes to in sources.list.
type Dist struct {
	// Suite is the dists/ subdirectory name, e.g. "stable" or "jammy".
	Suite string
	// Components are the sections of the suite, e.g. "main", "contrib".
	Components []string
	// Architectures are the binary architectures indexed, e.g. "amd64".
	// "source" is not listed here; SourceIndex tracks that separately.
	Architectures []string
}

// SuiteDir returns dists/<suite>.
func SuiteDir(suite string) string { return path.Join(DistsDir, suite) }

// ReleasePath returns the path of a suite's unsigned Release file.
func ReleasePath(suite string) string { return path.Join(SuiteDir(suite), "Release") }

// InReleasePath returns the path of a suite's inline-signed Release file, which
// is what apt prefers to fetch because it is a single object.
func InReleasePath(suite string) string { return path.Join(SuiteDir(suite), "InRelease") }

// ReleaseGPGPath returns the path of a suite's detached Release signature, kept
// alongside InRelease for clients that only understand the split form.
func ReleaseGPGPath(suite string) string { return path.Join(SuiteDir(suite), "Release.gpg") }

// BinaryIndexDir returns the directory holding a component's index for one
// architecture, relative to the repository root.
func BinaryIndexDir(suite, component, arch string) string {
	return path.Join(SuiteDir(suite), component, "binary-"+arch)
}

// SourceIndexDir returns the directory holding a component's source index.
func SourceIndexDir(suite, component string) string {
	return path.Join(SuiteDir(suite), component, ArchSource)
}

// IndexBase returns the uncompressed index filename for an architecture:
// "Sources" for the source pseudo-architecture, "Packages" otherwise.
func IndexBase(arch string) string {
	if arch == ArchSource {
		return "Sources"
	}
	return "Packages"
}

// IndexDir returns the index directory for any architecture, dispatching to
// SourceIndexDir for the source pseudo-architecture.
func IndexDir(suite, component, arch string) string {
	if arch == ArchSource {
		return SourceIndexDir(suite, component)
	}
	return BinaryIndexDir(suite, component, arch)
}

// ByHashPath returns the by-hash location of an index file: the index's
// directory, then by-hash/<ALGO>/<digest>.
//
// Publishing each index under its checksum as well as its plain name is what
// makes an apt publish near-atomic. A client that read the old Release keeps
// fetching the indexes that Release names, at paths no later publish reuses,
// so it never sees an index that does not match the Release it came from.
func ByHashPath(indexDir, algo, digest string) string {
	return path.Join(indexDir, ByHashDir, byHashAlgoDir(algo), digest)
}

// byHashAlgoDir is the directory name apt looks under for a given algorithm.
func byHashAlgoDir(algo string) string {
	switch algo {
	case HashMD5:
		return "MD5Sum"
	case HashSHA1:
		return "SHA1"
	default:
		return "SHA256"
	}
}

// PoolPath returns the conventional pool location of a file belonging to the
// named source package: pool/<component>/<prefix>/<source>/<filename>.
//
// The prefix is the source name's first letter, or its first four characters
// when it starts with "lib" — the split Debian uses to keep the thousands of
// library packages from landing in one directory.
func PoolPath(component, sourceName, filename string) string {
	return path.Join(PoolDir, component, PoolPrefix(sourceName), sourceName, filename)
}

// PoolDirFor returns the pool directory for a source package, without a file.
func PoolDirFor(component, sourceName string) string {
	return path.Join(PoolDir, component, PoolPrefix(sourceName), sourceName)
}

// PoolPrefix returns the pool hash-bucket directory for a source name.
func PoolPrefix(sourceName string) string {
	if sourceName == "" {
		return "_"
	}
	if strings.HasPrefix(sourceName, "lib") && len(sourceName) > 3 {
		return sourceName[:4]
	}
	return sourceName[:1]
}

// ValidSuite rejects suite names that would escape the dists/ directory or
// produce a path apt cannot address.
func ValidSuite(suite string) error { return validPathComponent("suite", suite, true) }

// ValidComponent rejects component names that would escape their suite.
// A component may contain a slash (Debian uses "updates/main"), so it is
// validated segment by segment.
func ValidComponent(component string) error {
	return validPathComponent("component", component, true)
}

// ValidArch rejects architecture names that are not plain identifiers.
func ValidArch(arch string) error { return validPathComponent("architecture", arch, false) }

// validPathComponent checks a name used as one or more path segments.
func validPathComponent(kind, name string, allowSlash bool) error {
	if name == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("%s %q must not start or end with a slash", kind, name)
	}
	segments := strings.Split(name, "/")
	if !allowSlash && len(segments) > 1 {
		return fmt.Errorf("%s %q must not contain a slash", kind, name)
	}
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%s %q contains an invalid path segment %q", kind, name, seg)
		}
		for _, r := range seg {
			if !isPathRune(r) {
				return fmt.Errorf("%s %q contains an invalid character %q", kind, name, r)
			}
		}
	}
	return nil
}

func isPathRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '-', r == '_', r == '.', r == '+':
		return true
	default:
		return false
	}
}
