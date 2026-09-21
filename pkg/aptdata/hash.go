package aptdata

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
)

// The hash algorithms an apt repository uses. SHA256 is the one apt verifies;
// MD5 and SHA1 are written for the benefit of older clients and are never
// trusted on their own.
const (
	HashMD5    = "md5"
	HashSHA1   = "sha1"
	HashSHA256 = "sha256"
)

// releaseBlock maps a hash algorithm to the Release field that carries it.
var releaseBlock = map[string]string{
	HashMD5:    "MD5Sum",
	HashSHA1:   "SHA1",
	HashSHA256: "SHA256",
}

// packagesField maps a hash algorithm to the Packages-index field that carries
// it for a binary package.
var packagesField = map[string]string{
	HashMD5:    FieldMD5sum,
	HashSHA1:   FieldSHA1,
	HashSHA256: FieldSHA256,
}

// DefaultHashes is the set written when none is chosen: SHA256 because apt
// requires it, MD5 because pre-1.0 apt and some mirroring tools still read it.
// SHA1 is omitted — apt rejects it as weak and it adds nothing alongside SHA256.
var DefaultHashes = []string{HashMD5, HashSHA256}

// ValidHash reports whether algo names a supported hash.
func ValidHash(algo string) bool {
	_, ok := releaseBlock[algo]
	return ok
}

// ParseHashes turns a comma-separated list such as "md5,sha256" into a
// normalized, deduplicated algorithm list in canonical order. SHA256 is always
// included: a Release file without it is unusable to modern apt.
func ParseHashes(s string) ([]string, error) {
	seen := map[string]bool{HashSHA256: true}
	for _, part := range strings.Split(s, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		if name == "sha512" {
			return nil, fmt.Errorf("hash %q is not used in apt Release files (use md5, sha1 or sha256)", name)
		}
		if !ValidHash(name) {
			return nil, fmt.Errorf("unknown hash %q (valid: md5, sha1, sha256)", name)
		}
		seen[name] = true
	}
	return canonicalHashOrder(seen), nil
}

// canonicalHashOrder returns the selected algorithms weakest-first, which is
// the order Debian's own Release files list the blocks in.
func canonicalHashOrder(seen map[string]bool) []string {
	var out []string
	for _, algo := range []string{HashMD5, HashSHA1, HashSHA256} {
		if seen[algo] {
			out = append(out, algo)
		}
	}
	return out
}

// Hashes holds one file's digests, keyed by algorithm.
type Hashes map[string]string

// newHash constructs the hash implementation for an algorithm.
func newHash(algo string) hash.Hash {
	switch algo {
	case HashMD5:
		return md5.New()
	case HashSHA1:
		return sha1.New()
	default:
		return sha256.New()
	}
}

// HashBytes computes the requested digests over data.
func HashBytes(data []byte, algos []string) Hashes {
	out := Hashes{}
	for _, algo := range algos {
		h := newHash(algo)
		h.Write(data)
		out[algo] = hex.EncodeToString(h.Sum(nil))
	}
	return out
}

// HashReader computes the requested digests over r in a single pass, returning
// them with the number of bytes read. Nothing is buffered, so it works on a
// package streaming past from remote storage.
func HashReader(r io.Reader, algos []string) (Hashes, int64, error) {
	if len(algos) == 0 {
		algos = DefaultHashes
	}
	hs := make(map[string]hash.Hash, len(algos))
	writers := make([]io.Writer, 0, len(algos))
	for _, algo := range algos {
		h := newHash(algo)
		hs[algo] = h
		writers = append(writers, h)
	}
	n, err := io.Copy(io.MultiWriter(writers...), r)
	if err != nil {
		return nil, n, err
	}
	out := Hashes{}
	for algo, h := range hs {
		out[algo] = hex.EncodeToString(h.Sum(nil))
	}
	return out, n, nil
}

// Algorithms returns the hash names present, in canonical order.
func (h Hashes) Algorithms() []string {
	seen := make(map[string]bool, len(h))
	for algo := range h {
		seen[algo] = true
	}
	return canonicalHashOrder(seen)
}

// sortedKeys returns a map's keys in order, for deterministic output.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
