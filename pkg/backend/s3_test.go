package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/logging"
)

func TestRegionEndpoint(t *testing.T) {
	for _, tc := range []struct{ region, want string }{
		{"eu-west-1", "https://s3.eu-west-1.amazonaws.com"},
		{"cn-north-1", "https://s3.cn-north-1.amazonaws.com.cn"},
		{"us-iso-east-1", "https://s3.us-iso-east-1.c2s.ic.gov"},
		{"us-isob-east-1", "https://s3.us-isob-east-1.sc2s.sgov.gov"},
	} {
		if got := regionEndpoint(tc.region); got != tc.want {
			t.Errorf("regionEndpoint(%q) = %q, want %q", tc.region, got, tc.want)
		}
	}
}

// testBackend builds an s3Backend whose every client talks to srv, so a region
// redirect can be observed without leaving the test process.
func testBackend(srv *httptest.Server, warn io.Writer) *s3Backend {
	s := &s3Backend{
		bucket:     "repo",
		label:      "s3://repo",
		configured: "us-east-1",
		region:     "us-east-1",
		warn:       warn,
		cfg: aws.Config{
			Region:      "us-east-1",
			Credentials: credentials.NewStaticCredentialsProvider("id", "secret", ""),
		},
		base: clientOptions(srv.URL),
	}
	s.client = s.newClient(s.region)
	return s
}

// An object uploaded in parts carries a composite checksum (the "-N" suffix),
// which the SDK cannot check against a whole-object download: it skips the
// check and logs a warning for every such GET. Reading a repository touches
// every object, so that warning drowns out the real output. Nothing here relies
// on it — content is hashed against the repository metadata instead — so the
// client must be built with the SDK's switch for it turned off.
func TestS3SuppressesMultipartChecksumWarning(t *testing.T) {
	const body = "payload"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-amz-checksum-crc32", "AAAAAA==-2")
		io.WriteString(w, body)
	}))
	defer srv.Close()

	// get reads an object through a client whose logger is captured, with the
	// suppression either on or off.
	get := func(t *testing.T, suppress bool) string {
		t.Helper()
		var logged bytes.Buffer
		s := testBackend(srv, io.Discard)
		s.cfg.Logger = logging.LoggerFunc(func(c logging.Classification, format string, v ...any) {
			fmt.Fprintf(&logged, "%s %s\n", c, fmt.Sprintf(format, v...))
		})
		// What LoadDefaultConfig resolves in real use, and what makes the SDK
		// ask S3 to return a checksum in the first place. Without it the
		// validation middleware never runs and the warning cannot appear.
		s.cfg.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenSupported
		s.base = func(o *s3.Options) {
			clientOptions(srv.URL)(o)
			o.DisableLogOutputChecksumValidationSkipped = suppress
		}
		s.client = s.newClient(s.region)

		rc, err := s.Get(context.Background(), "dists/stable/Release")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer rc.Close()
		if _, err := io.ReadAll(rc); err != nil {
			t.Fatalf("read: %v", err)
		}
		return logged.String()
	}

	if noisy := get(t, false); !strings.Contains(noisy, "multipart checksum") {
		t.Skipf("this SDK build does not log the multipart-checksum warning, so there is nothing to suppress; it logged: %q", noisy)
	}
	if quiet := get(t, true); strings.Contains(quiet, "multipart checksum") {
		t.Errorf("the multipart-checksum warning is still being logged:\n%s", quiet)
	}

	// The option has to survive every path that builds a client, including the
	// rebuild that follows a region redirect.
	s := testBackend(srv, io.Discard)
	for name, c := range map[string]*s3.Client{"initial": s.client, "after redirect": s.newClient("eu-west-1")} {
		if !c.Options().DisableLogOutputChecksumValidationSkipped {
			t.Errorf("%s client does not suppress the multipart-checksum warning", name)
		}
	}
}

const redirectBody = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<Error><Code>PermanentRedirect</Code>` +
	`<Message>The bucket you are attempting to access must be addressed using the specified endpoint.</Message>` +
	`</Error>`

func TestS3FollowsRegionRedirect(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-amz-bucket-region", "eu-west-1")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusMovedPermanently)
			io.WriteString(w, redirectBody)
			return
		}
		io.WriteString(w, "payload")
	}))
	defer srv.Close()

	var warn bytes.Buffer
	s := testBackend(srv, &warn)

	rc, err := s.Get(context.Background(), "dists/stable/Release")
	if err != nil {
		t.Fatalf("Get after redirect: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "payload" {
		t.Errorf("body = %q, want %q", body, "payload")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server calls = %d, want 2 (redirect then retry)", got)
	}
	if s.region != "eu-west-1" {
		t.Errorf("region = %q, want eu-west-1 to be reused for later calls", s.region)
	}
	for _, want := range []string{"eu-west-1", "us-east-1", "https://s3.eu-west-1.amazonaws.com"} {
		if !strings.Contains(warn.String(), want) {
			t.Errorf("warning %q does not mention %q", warn.String(), want)
		}
	}

	// A second operation goes straight to the corrected region: one call, no
	// further warning.
	warn.Reset()
	calls.Store(1) // the handler only redirects on its first call
	if _, err := s.Stat(context.Background(), "dists/stable/Release"); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server calls = %d, want 1 extra call", got-1)
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected second warning: %q", warn.String())
	}
}

func TestS3DoesNotRetryOnSameRegion(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Right region, but the object is missing: nothing to correct.
		w.Header().Set("x-amz-bucket-region", "us-east-1")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var warn bytes.Buffer
	s := testBackend(srv, &warn)
	if _, err := s.Get(context.Background(), "missing"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Get = %v, want ErrNotExist", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server calls = %d, want 1 (no retry)", got)
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warning: %q", warn.String())
	}
}

func TestS3PinnedEndpointIgnoresRedirect(t *testing.T) {
	var warn bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	s := testBackend(srv, &warn)
	s.pinned = true
	if s.useRegion("eu-west-1") {
		t.Error("useRegion changed region despite an explicit AWS_ENDPOINT_URL")
	}
	if s.region != "us-east-1" {
		t.Errorf("region = %q, want it left alone", s.region)
	}
}
