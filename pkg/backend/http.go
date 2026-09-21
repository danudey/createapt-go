package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// errReadOnly is returned by write operations on read-only backends.
var errReadOnly = errors.New("backend: location is read-only")

// httpBackend is a read-only Backend over HTTP(S). It supports inspecting and
// reading a published repository but cannot be used as a publish target.
type httpBackend struct {
	base   string // base URL with a trailing slash
	client *http.Client
}

func newHTTP(location string) *httpBackend {
	if !strings.HasSuffix(location, "/") {
		location += "/"
	}
	return &httpBackend{base: location, client: http.DefaultClient}
}

func (h *httpBackend) url(relpath string) string {
	return h.base + strings.TrimLeft(relpath, "/")
}

func (h *httpBackend) Get(ctx context.Context, relpath string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url(relpath), nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		drainClose(resp.Body)
		return nil, ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		drainClose(resp.Body)
		return nil, fmt.Errorf("backend: GET %s: %s", h.url(relpath), resp.Status)
	}
	return resp.Body, nil
}

func (h *httpBackend) Stat(ctx context.Context, relpath string) (*FileInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, h.url(relpath), nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer drainClose(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend: HEAD %s: %s", h.url(relpath), resp.Status)
	}
	return &FileInfo{Size: resp.ContentLength}, nil
}

func (h *httpBackend) Put(context.Context, string, io.Reader, int64) error { return errReadOnly }
func (h *httpBackend) Delete(context.Context, string) error                { return errReadOnly }
func (h *httpBackend) String() string                                      { return h.base }
