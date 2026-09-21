package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
)

// sha256Seek hashes the contents of rs from the beginning and leaves it
// re-positioned at the start, ready to be read again (e.g. for upload).
func sha256Seek(rs io.ReadSeeker) (string, error) {
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, rs); err != nil {
		return "", err
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
