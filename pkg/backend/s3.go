package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// metaSHA256 is the object-metadata key under which we record an uploaded
// object's sha256 (hex). Storing it lets Hash validate a remote package without
// downloading it.
const metaSHA256 = "sha256"

// probeRegion is used only to sign the initial HeadBucket when no region is
// configured at all: a request must name some region to be signed, and the
// bucket's reply tells us the real one (see detectRegion).
const probeRegion = "us-east-1"

// s3Backend operates on a repository stored under a prefix in an S3 bucket.
type s3Backend struct {
	bucket string
	prefix string
	label  string

	cfg    aws.Config
	base   func(*s3.Options) // options common to every client we build
	pinned bool              // AWS_ENDPOINT_URL set: never chase region redirects
	warn   io.Writer         // where region warnings go; os.Stderr in practice

	// configured is the region the user asked for (AWS_REGION / --region), kept
	// so a redirect can report what was wrong. Empty if none was set.
	configured string

	mu     sync.Mutex
	region string // region the current client talks to
	client *s3.Client
}

func newS3(ctx context.Context, location string) (*s3Backend, error) {
	bucket, prefix, err := parseBucketURL(location)
	if err != nil {
		return nil, err
	}
	// StdinTokenProvider lets profiles that assume a role guarded by MFA
	// (mfa_serial in ~/.aws/config) work interactively: the SDK prompts for the
	// current MFA code on stdin instead of failing with "AssumeRoleTokenProvider
	// session option not set". Region and profile selection are left to the
	// standard AWS_REGION / AWS_PROFILE environment variables (see --region /
	// --profile), which LoadDefaultConfig already honours.
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithAssumeRoleCredentialOptions(func(o *stscreds.AssumeRoleOptions) {
			o.TokenProvider = stscreds.StdinTokenProvider
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("s3: load config: %w", err)
	}
	// Keep the resulting session credentials on disk so the next command reuses
	// them instead of asking for another MFA code (see awscreds.go).
	withCredentialCache(ctx, &cfg, os.Stderr)
	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	s := &s3Backend{
		bucket:     bucket,
		prefix:     prefix,
		label:      location,
		cfg:        cfg,
		configured: cfg.Region,
		pinned:     endpoint != "",
		warn:       os.Stderr,
		region:     cfg.Region,
		base:       clientOptions(endpoint),
	}
	if s.region == "" {
		s.region = probeRegion
	}
	s.client = s.newClient(s.region)
	if !s.pinned {
		s.detectRegion(ctx)
	}
	return s, nil
}

// clientOptions returns the option adjustments applied to every S3 client this
// backend builds. endpoint, when non-empty, points the client at an
// S3-compatible store (MinIO and friends) instead of AWS.
func clientOptions(endpoint string) func(*s3.Options) {
	return func(o *s3.Options) {
		// Silence the SDK's per-object complaints that it did not validate a
		// response checksum. It emits one of two warnings on every GET it
		// cannot check: "Skipped validation of multipart checksum." for an
		// object uploaded in parts (its composite checksum cannot be checked
		// against a whole-object download), and "Response has no supported
		// checksum." for an object stored without one. This single option
		// covers both, which is what we want: nothing here relies on the SDK's
		// response checksum. Every package and metadata file is hashed as it is
		// read and compared against the checksum the repository metadata
		// records — a stronger check, and the one whose failure is actually
		// reported. Reading a repository touches every object, so without this
		// the real output is buried.
		o.DisableLogOutputChecksumValidationSkipped = true

		// Allow overriding the endpoint for S3-compatible stores (MinIO etc.).
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		}
	}
}

// newClient builds a client for one region, keeping the shared options.
func (s *s3Backend) newClient(region string) *s3.Client {
	return s3.NewFromConfig(s.cfg, s.base, func(o *s3.Options) { o.Region = region })
}

// current returns the client to use for the next request.
func (s *s3Backend) current() *s3.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// detectRegion asks the bucket where it lives before the first real request, so
// a wrong region costs one HeadBucket instead of an opaque PermanentRedirect.
// S3 returns the x-amz-bucket-region header even when it rejects the HeadBucket
// (a 301 from the wrong region, or a 403 when we may not head the bucket), so a
// failed probe is still informative. Learning nothing is not fatal: we keep the
// configured region and let the real operation report its own error.
func (s *s3Backend) detectRegion(ctx context.Context) {
	out, err := s.current().HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err == nil {
		s.useRegion(aws.ToString(out.BucketRegion))
		return
	}
	s.useRegion(bucketRegionOf(err))
}

// useRegion repoints the backend at region for the rest of the run and reports
// true if it did. It warns when this corrects a region the user actually asked
// for; adopting a region we only guessed at (nothing configured) is routine and
// stays quiet.
func (s *s3Backend) useRegion(region string) bool {
	if s.pinned || region == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if region == s.region {
		return false
	}
	if s.configured != "" && s.warn != nil {
		fmt.Fprintf(s.warn, "warning: bucket %q is in region %s, not %s; using endpoint %s for the rest of this run\n",
			s.bucket, region, s.region, regionEndpoint(region))
	}
	s.region = region
	s.client = s.newClient(region)
	return true
}

// bucketRegionOf reads the bucket's real region out of a failed S3 response.
// S3 answers a request aimed at the wrong region with "PermanentRedirect: ...
// must be addressed using the specified endpoint", naming no endpoint, but the
// response always carries the region in x-amz-bucket-region.
func bucketRegionOf(err error) string {
	var re *awshttp.ResponseError
	if !errors.As(err, &re) || re.Response == nil {
		return ""
	}
	return re.Response.Header.Get("x-amz-bucket-region")
}

// regionEndpoint renders the S3 endpoint serving a region, so a warning can
// name the endpoint the redirect itself omits.
func regionEndpoint(region string) string {
	suffix := "amazonaws.com"
	switch {
	case strings.HasPrefix(region, "cn-"):
		suffix = "amazonaws.com.cn"
	case strings.HasPrefix(region, "us-isob-"):
		suffix = "sc2s.sgov.gov"
	case strings.HasPrefix(region, "us-iso-"):
		suffix = "c2s.ic.gov"
	}
	return fmt.Sprintf("https://s3.%s.%s", region, suffix)
}

// s3do runs one S3 operation, retrying it once against the bucket's real region
// if S3 redirects us. Every call goes through here so a redirect met part-way
// through a run (or on a bucket we never probed) is corrected in place rather
// than failing the command.
func s3do[T any](s *s3Backend, fn func(*s3.Client) (T, error)) (T, error) {
	out, err := fn(s.current())
	if err == nil {
		return out, nil
	}
	if s.useRegion(bucketRegionOf(err)) {
		return fn(s.current())
	}
	return out, err
}

func (s *s3Backend) key(relpath string) string { return joinPrefix(s.prefix, relpath) }

func isS3NotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

func (s *s3Backend) Get(ctx context.Context, relpath string) (io.ReadCloser, error) {
	out, err := s3do(s, func(c *s3.Client) (*s3.GetObjectOutput, error) {
		return c.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(s.key(relpath)),
		})
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	return out.Body, nil
}

func (s *s3Backend) Stat(ctx context.Context, relpath string) (*FileInfo, error) {
	out, err := s3do(s, func(c *s3.Client) (*s3.HeadObjectOutput, error) {
		return c.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(s.key(relpath)),
		})
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	fi := &FileInfo{}
	if out.ContentLength != nil {
		fi.Size = *out.ContentLength
	}
	if sum, ok := out.Metadata[metaSHA256]; ok && sum != "" {
		fi.Checksum = sum
		fi.ChecksumType = AlgoSHA256
	}
	return fi, nil
}

func (s *s3Backend) Put(ctx context.Context, relpath string, r io.Reader, size int64) error {
	body, sum, cleanup, err := seekableWithSum(r)
	if err != nil {
		return err
	}
	defer cleanup()
	meta := map[string]string{}
	if sum != "" {
		meta[metaSHA256] = sum
	}
	in := &s3.PutObjectInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(s.key(relpath)),
		Body:     body,
		Metadata: meta,
	}
	if size >= 0 {
		in.ContentLength = aws.Int64(size)
	}
	// Rewind per attempt: a retry after a region redirect re-sends the body.
	_, err = s3do(s, func(c *s3.Client) (*s3.PutObjectOutput, error) {
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return c.PutObject(ctx, in)
	})
	return err
}

func (s *s3Backend) Delete(ctx context.Context, relpath string) error {
	_, err := s3do(s, func(c *s3.Client) (*s3.DeleteObjectOutput, error) {
		return c.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(s.key(relpath)),
		})
	})
	if err != nil && isS3NotFound(err) {
		return nil
	}
	return err
}

func (s *s3Backend) String() string { return s.label }

// Copy duplicates src to dst within the bucket using a server-side CopyObject,
// so the object bytes never transit this process. The default copy directive
// preserves the source's user metadata (including the sha256 recorded at upload
// time), so Hash keeps working on the destination.
func (s *s3Backend) Copy(ctx context.Context, src, dst string) error {
	if _, err := s.Stat(ctx, src); err != nil {
		return err // maps NoSuchKey to ErrNotExist
	}
	_, err := s3do(s, func(c *s3.Client) (*s3.CopyObjectOutput, error) {
		return c.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket:     aws.String(s.bucket),
			Key:        aws.String(s.key(dst)),
			CopySource: aws.String(encodeCopySource(s.bucket, s.key(src))),
		})
	})
	if err != nil {
		if isS3NotFound(err) {
			return ErrNotExist
		}
		return err
	}
	return nil
}

// encodeCopySource builds the x-amz-copy-source value ("bucket/key") with each
// key path segment URL-encoded (so characters such as '+' survive) while
// leaving the separators intact.
func encodeCopySource(bucket, key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return bucket + "/" + strings.Join(segs, "/")
}

// List implements Lister by paging through ListObjectsV2 under the given
// repo-relative prefix, returning each object as a repo-relative path (the
// bucket prefix stripped) with its size.
func (s *s3Backend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	keyPrefix := s.key(prefix)
	strip := ""
	if s.prefix != "" {
		strip = s.prefix + "/"
	}
	// The paginator is built inside the call so a region redirect restarts the
	// listing cleanly against the new client.
	return s3do(s, func(c *s3.Client) ([]ObjectInfo, error) {
		var out []ObjectInfo
		pager := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{
			Bucket: aws.String(s.bucket),
			Prefix: aws.String(keyPrefix),
		})
		for pager.HasMorePages() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, obj := range page.Contents {
				key := aws.ToString(obj.Key)
				rel := strings.TrimPrefix(key, strip)
				var size int64
				if obj.Size != nil {
					size = *obj.Size
				}
				out = append(out, ObjectInfo{Path: rel, Size: size})
			}
		}
		return out, nil
	})
}

// HashesContent reports false: Hash plays back the checksum recorded at upload
// time, so it proves what was uploaded but not that the object still holds it.
// S3 will not compute a sha256 over stored content on demand.
func (s *s3Backend) HashesContent() bool { return false }

// Hash implements RemoteHasher using the sha256 we stored in object metadata at
// upload time. Returns ("", false, nil) when the object predates our metadata.
func (s *s3Backend) Hash(ctx context.Context, relpath, algo string) (string, bool, error) {
	if algo != AlgoSHA256 {
		return "", false, nil
	}
	fi, err := s.Stat(ctx, relpath)
	if err != nil {
		return "", false, err
	}
	if fi.Checksum == "" {
		return "", false, nil
	}
	return fi.Checksum, true, nil
}

// seekableWithSum returns a seekable body positioned at the start, plus the
// sha256 (hex) of its contents. Object stores need a seekable body to sign the
// request and we get the checksum "for free" in the same pass. The returned
// cleanup must be called when done.
func seekableWithSum(r io.Reader) (body io.ReadSeeker, sum string, cleanup func(), err error) {
	if rs, ok := r.(io.ReadSeeker); ok {
		s, err := sha256Seek(rs)
		return rs, s, func() {}, err
	}
	// Spill arbitrary readers to a temp file so we can seek and hash.
	tmp, err := os.CreateTemp("", "cr-upload-*")
	if err != nil {
		return nil, "", func() {}, err
	}
	cleanup = func() { tmp.Close(); os.Remove(tmp.Name()) }
	if _, err := io.Copy(tmp, r); err != nil {
		cleanup()
		return nil, "", func() {}, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, "", func() {}, err
	}
	s, err := sha256Seek(tmp)
	if err != nil {
		cleanup()
		return nil, "", func() {}, err
	}
	return tmp, s, cleanup, nil
}
