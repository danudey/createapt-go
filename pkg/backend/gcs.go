package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// gcsBackend operates on a repository stored under a prefix in a Google Cloud
// Storage bucket. Like the S3 backend it records each object's sha256 in custom
// metadata so an existing package can be validated without downloading it.
type gcsBackend struct {
	client *storage.Client
	bucket string
	prefix string
	label  string
}

func newGCS(ctx context.Context, location string) (*gcsBackend, error) {
	bucket, prefix, err := parseBucketURL(location)
	if err != nil {
		return nil, err
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs: new client: %w", err)
	}
	return &gcsBackend{client: client, bucket: bucket, prefix: prefix, label: location}, nil
}

func (g *gcsBackend) obj(relpath string) *storage.ObjectHandle {
	return g.client.Bucket(g.bucket).Object(joinPrefix(g.prefix, relpath))
}

func (g *gcsBackend) Get(ctx context.Context, relpath string) (io.ReadCloser, error) {
	rc, err := g.obj(relpath).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, ErrNotExist
	}
	return rc, err
}

func (g *gcsBackend) Stat(ctx context.Context, relpath string) (*FileInfo, error) {
	attrs, err := g.obj(relpath).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	fi := &FileInfo{Size: attrs.Size}
	if sum := attrs.Metadata[metaSHA256]; sum != "" {
		fi.Checksum = sum
		fi.ChecksumType = AlgoSHA256
	}
	return fi, nil
}

func (g *gcsBackend) Put(ctx context.Context, relpath string, r io.Reader, _ int64) error {
	body, sum, cleanup, err := seekableWithSum(r)
	if err != nil {
		return err
	}
	defer cleanup()
	w := g.obj(relpath).NewWriter(ctx)
	if sum != "" {
		w.Metadata = map[string]string{metaSHA256: sum}
	}
	if _, err := io.Copy(w, body); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

func (g *gcsBackend) Delete(ctx context.Context, relpath string) error {
	err := g.obj(relpath).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return err
}

func (g *gcsBackend) String() string { return g.label }

func (g *gcsBackend) Close() error { return g.client.Close() }

// Copy duplicates src to dst within the bucket using a server-side copy, so the
// object bytes never transit this process. The copy carries the source's
// metadata (including the sha256 recorded at upload time) across by default.
func (g *gcsBackend) Copy(ctx context.Context, src, dst string) error {
	from := g.obj(src)
	if _, err := from.Attrs(ctx); err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return ErrNotExist
		}
		return err
	}
	_, err := g.obj(dst).CopierFrom(from).Run(ctx)
	return err
}

// List implements Lister by iterating the bucket under the given repo-relative
// prefix, returning each object as a repo-relative path (the bucket prefix
// stripped) with its size.
func (g *gcsBackend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	objPrefix := joinPrefix(g.prefix, prefix)
	strip := ""
	if g.prefix != "" {
		strip = g.prefix + "/"
	}
	var out []ObjectInfo
	it := g.client.Bucket(g.bucket).Objects(ctx, &storage.Query{Prefix: objPrefix})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, ObjectInfo{Path: strings.TrimPrefix(attrs.Name, strip), Size: attrs.Size})
	}
	return out, nil
}

// HashesContent reports false: Hash plays back the checksum recorded at upload
// time, so it proves what was uploaded but not that the object still holds it.
func (g *gcsBackend) HashesContent() bool { return false }

// Hash implements RemoteHasher from the stored sha256 metadata.
func (g *gcsBackend) Hash(ctx context.Context, relpath, algo string) (string, bool, error) {
	if algo != AlgoSHA256 {
		return "", false, nil
	}
	fi, err := g.Stat(ctx, relpath)
	if err != nil {
		return "", false, err
	}
	if fi.Checksum == "" {
		return "", false, nil
	}
	return fi.Checksum, true, nil
}
