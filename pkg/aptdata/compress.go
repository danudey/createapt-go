package aptdata

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// The filename extensions the compressed forms use.
const (
	extGZIP = ".gz"
	extXZ   = ".xz"
	extZSTD = ".zst"
	extBZ2  = ".bz2"
)

// Compression names one of the forms an index is published in. An index is
// normally written in several at once, because which one a client fetches
// depends on its apt version.
type Compression string

// The compression forms this tool writes.
const (
	// None publishes the index uncompressed. apt still accepts it, and it is
	// what a by-hash fallback and a human reading the repository want.
	None Compression = "none"
	// GZIP is understood by every apt ever shipped.
	GZIP Compression = "gzip"
	// XZ is what Debian and Ubuntu publish; apt has read it since 0.9.
	XZ Compression = "xz"
	// ZSTD is read by apt 2.3+ (Ubuntu 22.04, Debian 12) and nothing older.
	ZSTD Compression = "zstd"
)

// DefaultCompressions is the set written when none is chosen: the plain index
// plus gzip and xz, which between them cover every apt in service.
var DefaultCompressions = []Compression{None, GZIP, XZ}

// Ext returns the filename extension for the compression, empty for None.
func (c Compression) Ext() string {
	switch c {
	case GZIP:
		return extGZIP
	case XZ:
		return extXZ
	case ZSTD:
		return extZSTD
	default:
		return ""
	}
}

// Valid reports whether c names a supported compression.
func (c Compression) Valid() bool {
	switch c {
	case None, GZIP, XZ, ZSTD:
		return true
	default:
		return false
	}
}

// ParseCompressions turns a comma-separated list such as "gzip,xz" into a
// normalized, deduplicated list. The uncompressed form is always included: it
// is the only one every client and every mirroring tool can read, and it costs
// one small object per index.
func ParseCompressions(s string) ([]Compression, error) {
	seen := map[Compression]bool{None: true}
	for _, part := range strings.Split(s, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		switch name {
		case "":
			continue
		case "gz":
			name = string(GZIP)
		case "zst":
			name = string(ZSTD)
		case "plain", "uncompressed":
			name = string(None)
		}
		c := Compression(name)
		if !c.Valid() {
			return nil, fmt.Errorf("unknown compression %q (valid: none, gzip, xz, zstd)", part)
		}
		seen[c] = true
	}
	var out []Compression
	for _, c := range []Compression{None, GZIP, XZ, ZSTD} {
		if seen[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

// Compress encodes data in the named form. None returns the input unchanged.
func (c Compression) Compress(data []byte) ([]byte, error) {
	switch c {
	case None:
		return data, nil
	case GZIP:
		var buf bytes.Buffer
		// Best compression: indexes are written once and fetched by every
		// client, so the extra CPU on publish is repaid many times over.
		w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case XZ:
		var buf bytes.Buffer
		w, err := xz.NewWriter(&buf)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case ZSTD:
		var buf bytes.Buffer
		w, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("unsupported compression %q", c)
	}
}

// Decompress decodes an index whose compression is inferred from its filename
// extension. An unrecognized extension is treated as uncompressed.
//
// Nothing here shells out: gzip, bzip2, xz and zstd are all decoded in process,
// so a repository can be read on a host with no compression utilities
// installed.
func Decompress(name string, data []byte) ([]byte, error) {
	switch strings.ToLower(path.Ext(name)) {
	case extGZIP:
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("gzip %s: %w", name, err)
		}
		defer r.Close()
		return io.ReadAll(r)
	case extXZ:
		r, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("xz %s: %w", name, err)
		}
		return io.ReadAll(r)
	case extZSTD, ".zstd":
		r, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("zstd %s: %w", name, err)
		}
		defer r.Close()
		return io.ReadAll(r)
	case extBZ2:
		return io.ReadAll(bzip2.NewReader(bytes.NewReader(data)))
	default:
		return data, nil
	}
}

// DecompressStream wraps r with a decoder chosen from name's extension. The
// caller closes the returned reader. It is used where an index is large enough
// that buffering it whole would be wasteful.
func DecompressStream(name string, r io.Reader) (io.ReadCloser, error) {
	switch strings.ToLower(path.Ext(name)) {
	case extGZIP:
		return gzip.NewReader(r)
	case extXZ:
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(xr), nil
	case extZSTD, ".zstd":
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return zr.IOReadCloser(), nil
	case extBZ2:
		return io.NopCloser(bzip2.NewReader(r)), nil
	default:
		return io.NopCloser(r), nil
	}
}
