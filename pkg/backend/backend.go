// Package backend abstracts the storage that holds a repository so the same
// repository logic works against a local directory, an SSH/SFTP host, an S3
// bucket, a GCS bucket, or (read-only) an HTTP server. Paths are always
// relative to the repository root and use forward slashes.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// ErrNotExist is returned by Get and Stat when the named object is absent.
var ErrNotExist = errors.New("backend: object does not exist")

// FileInfo describes a stored object.
type FileInfo struct {
	Size int64
	// Checksum and ChecksumType are populated when the backend can report a
	// content hash cheaply (e.g. stored object metadata). Empty otherwise.
	Checksum     string
	ChecksumType string
}

// Backend is the minimal storage interface a repository needs.
type Backend interface {
	// Get opens an object for reading. Returns ErrNotExist if absent.
	Get(ctx context.Context, relpath string) (io.ReadCloser, error)
	// Stat reports an object's size (and checksum if known). Returns
	// ErrNotExist if absent.
	Stat(ctx context.Context, relpath string) (*FileInfo, error)
	// Put writes an object, creating parent directories as needed. size may be
	// -1 if unknown.
	Put(ctx context.Context, relpath string, r io.Reader, size int64) error
	// Delete removes an object. Removing a missing object is not an error.
	Delete(ctx context.Context, relpath string) error
	// String returns a human-readable location for diagnostics.
	String() string
}

// AlgoSHA256 names the only content hash the backends implement. It is the
// algorithm argument to RemoteHasher.Hash and the value reported in
// FileInfo.ChecksumType.
const AlgoSHA256 = "sha256"

// RemoteHasher is an optional capability: a backend that can compute (or look
// up) the checksum of a stored object without transferring its contents. This
// lets the repository verify that an already-present package matches a local file
// without downloading it. algo is a hash name such as AlgoSHA256.
type RemoteHasher interface {
	// Hash returns the object's checksum and true, or ("", false, nil) if the
	// checksum cannot be determined without downloading.
	Hash(ctx context.Context, relpath, algo string) (string, bool, error)
}

// ContentHasher is implemented by a RemoteHasher whose Hash results are read
// from the stored object's actual bytes rather than from a checksum recorded
// alongside it when it was uploaded. The distinction matters to verification: a
// recorded checksum proves what the uploader claimed, and so catches a
// repository whose metadata and objects disagree, but it cannot detect an
// object whose content has since changed underneath it. Only a hash over the
// content itself proves the object still matches its metadata.
type ContentHasher interface {
	RemoteHasher
	// HashesContent reports whether Hash re-reads the object's bytes.
	HashesContent() bool
}

// HashesContent reports whether be can prove an object's checksum from its
// content without the caller transferring it. A backend that is not a
// RemoteHasher, or one that only plays back a recorded checksum, reports false.
func HashesContent(be Backend) bool {
	h, ok := be.(ContentHasher)
	return ok && h.HashesContent()
}

// ObjectInfo names a stored object and its size, as returned by Lister.List.
// Path is repo-relative and uses forward slashes.
type ObjectInfo struct {
	Path string
	Size int64
}

// Lister is an optional capability: a backend that can enumerate the objects it
// stores. It lets callers reconcile the physical layout against the metadata —
// summing the repository's total size, or finding package/metadata files that the
// published metadata no longer references. Read-only transports that cannot list
// (e.g. plain HTTP) do not implement it.
type Lister interface {
	// List returns every object whose repo-relative path is under prefix
	// (recursively). A prefix of "" lists the whole repository. Directories
	// themselves are not returned, only the files within them.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
}

// Copier is an optional capability: a backend that can duplicate an object
// within its own storage without transferring the bytes through this process.
// It lets a relocated package (for example one moved under a new
// --location-prefix) be republished without re-uploading an identical copy:
// the object is copied to its new location before the metadata is swapped, and
// the old copy is removed during garbage collection afterwards.
type Copier interface {
	// Copy duplicates src to dst within the backend, overwriting dst and
	// creating dst's parent directories as needed. It returns ErrNotExist if
	// src does not exist.
	Copy(ctx context.Context, src, dst string) error
}

// FileStore is an optional capability: a backend whose objects are ordinary
// files on this machine's filesystem, so a caller that needs to open one as a
// file can do so in place instead of transferring it to a temporary copy. It
// is what distinguishes a repository whose packages are free to re-read from
// one where reading them means a download.
type FileStore interface {
	// LocalPath returns the filesystem path of the named object. The object
	// need not exist.
	LocalPath(relpath string) string
}

// LocalPath returns the filesystem path of an object when be keeps its objects
// as local files, and ("", false) otherwise.
func LocalPath(be Backend, relpath string) (string, bool) {
	fs, ok := be.(FileStore)
	if !ok {
		return "", false
	}
	return fs.LocalPath(relpath), true
}

// IsLocal reports whether a backend's objects are local files.
func IsLocal(be Backend) bool {
	_, ok := be.(FileStore)
	return ok
}

// Closer is implemented by backends holding connections that should be closed.
type Closer interface {
	Close() error
}

// Open creates a backend for the given repository location. Supported schemes:
//
//	(none) or file://   local filesystem
//	sftp://user@host[:port]/path
//	s3://bucket/prefix
//	gs://bucket/prefix
//	http(s)://host/path  (read-only)
func Open(ctx context.Context, location string) (Backend, error) {
	scheme, rest := splitScheme(location)
	switch scheme {
	case "", "file":
		return newLocal(rest)
	case "sftp", "ssh":
		return newSFTP(ctx, location)
	case "s3":
		return newS3(ctx, location)
	case "gs", "gcs":
		return newGCS(ctx, location)
	case "http", "https":
		return newHTTP(location), nil
	default:
		return nil, fmt.Errorf("backend: unsupported scheme %q in %q", scheme, location)
	}
}

// splitScheme separates a "scheme://" prefix. For file paths with no scheme it
// returns ("", path). For file:// it returns ("file", path-with-leading-slash).
func splitScheme(location string) (scheme, rest string) {
	before, after, ok := strings.Cut(location, "://")
	if !ok {
		return "", location
	}
	return before, after
}

// parseBucketURL parses s3://bucket/prefix or gs://bucket/prefix into bucket
// and a (possibly empty) prefix with no leading or trailing slash.
func parseBucketURL(location string) (bucket, prefix string, err error) {
	u, err := url.Parse(location)
	if err != nil {
		return "", "", err
	}
	bucket = u.Host
	if bucket == "" {
		return "", "", fmt.Errorf("backend: missing bucket in %q", location)
	}
	prefix = strings.Trim(u.Path, "/")
	return bucket, prefix, nil
}

// joinPrefix joins a bucket/host prefix with a repo-relative path.
func joinPrefix(prefix, relpath string) string {
	relpath = strings.TrimLeft(relpath, "/")
	if prefix == "" {
		return relpath
	}
	return prefix + "/" + relpath
}

// drainClose closes r, ignoring errors; used in defers.
func drainClose(r io.Closer) { _ = r.Close() }
