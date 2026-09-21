package debmeta

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The ar container a .deb is built from. Only the common-format subset Debian
// uses is implemented — plain names, decimal sizes, no GNU or BSD extended
// name tables — because dpkg-deb writes nothing else and a .deb that needed
// them would not be installable.
const (
	arMagic       = "!<arch>\n"
	arHeaderSize  = 60
	arFileMagic   = "`\n"
	arNameLen     = 16
	arSizeOffset  = 48
	arSizeLen     = 10
	arMagicOffset = 58
)

// errArEnd signals a clean end of the archive.
var errArEnd = errors.New("ar: end of archive")

// arReader walks the members of an ar archive in order.
type arReader struct {
	r       io.Reader
	current int64 // bytes remaining in the current member
	padding bool  // whether the current member needs a padding byte skipped
}

// arMember describes one archive member.
type arMember struct {
	Name string
	Size int64
}

// newArReader validates the global header and returns a reader positioned at
// the first member.
func newArReader(r io.Reader) (*arReader, error) {
	magic := make([]byte, len(arMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, fmt.Errorf("read ar magic: %w", err)
	}
	if string(magic) != arMagic {
		return nil, fmt.Errorf("not an ar archive (bad magic %q)", magic)
	}
	return &arReader{r: r}, nil
}

// Next advances to the next member, skipping any unread bytes of the current
// one. It returns errArEnd at the end of the archive.
func (a *arReader) Next() (*arMember, error) {
	if err := a.skipCurrent(); err != nil {
		return nil, err
	}

	header := make([]byte, arHeaderSize)
	if _, err := io.ReadFull(a.r, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errArEnd
		}
		return nil, fmt.Errorf("read ar header: %w", err)
	}
	if string(header[arMagicOffset:arMagicOffset+2]) != arFileMagic {
		return nil, fmt.Errorf("malformed ar header (bad terminator)")
	}

	// A name is padded with spaces and, by convention, terminated with '/'.
	name := strings.TrimRight(string(header[:arNameLen]), " ")
	name = strings.TrimSuffix(name, "/")

	sizeField := strings.TrimSpace(string(header[arSizeOffset : arSizeOffset+arSizeLen]))
	size, err := strconv.ParseInt(sizeField, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("malformed ar member size %q: %w", sizeField, err)
	}
	if size < 0 {
		return nil, fmt.Errorf("negative ar member size %d", size)
	}

	a.current = size
	// Members are aligned to an even offset.
	a.padding = size%2 == 1
	return &arMember{Name: name, Size: size}, nil
}

// Read reads from the current member, stopping at its end.
func (a *arReader) Read(p []byte) (int, error) {
	if a.current <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > a.current {
		p = p[:a.current]
	}
	n, err := a.r.Read(p)
	a.current -= int64(n)
	return n, err
}

// skipCurrent discards the rest of the current member and its padding.
func (a *arReader) skipCurrent() error {
	if a.current > 0 {
		if _, err := io.CopyN(io.Discard, a.r, a.current); err != nil {
			return fmt.Errorf("skip ar member: %w", err)
		}
		a.current = 0
	}
	if a.padding {
		if _, err := io.CopyN(io.Discard, a.r, 1); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("skip ar padding: %w", err)
		}
		a.padding = false
	}
	return nil
}
