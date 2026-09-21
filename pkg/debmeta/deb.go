// Package debmeta extracts repository metadata from Debian package files: the
// control stanza of a binary .deb, and the source stanza described by a .dsc
// and the tarballs it names.
//
// Everything here is pure Go — the ar container, the compressed control
// tarball, and the deb822 control file are all read in process — so a
// repository can be built on a host with no dpkg installed, which is what makes
// remote (S3/GCS/SFTP) targets practical.
package debmeta

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/danudey/createapt-go/pkg/aptdata"
)

// Options configures metadata extraction.
type Options struct {
	// Hashes are the digest algorithms to compute over the package file. When
	// empty, aptdata.DefaultHashes is used. SHA256 is always computed: it is
	// the package's identity within the repository.
	Hashes []string

	// Component is the section of the suite the package will be indexed in. It
	// decides the pool subdirectory the package is placed under.
	Component string

	// PoolLayout, when false, places the package directly under Component's
	// pool directory instead of the conventional
	// pool/<component>/<prefix>/<source>/ tree.
	PoolLayout bool
}

func (o Options) hashes() []string {
	if len(o.Hashes) == 0 {
		return aptdata.DefaultHashes
	}
	seen := map[string]bool{aptdata.HashSHA256: true}
	for _, h := range o.Hashes {
		seen[h] = true
	}
	out := make([]string, 0, len(seen))
	for _, algo := range []string{aptdata.HashMD5, aptdata.HashSHA1, aptdata.HashSHA256} {
		if seen[algo] {
			out = append(out, algo)
		}
	}
	return out
}

// The extensions a binary package file carries.
const (
	extDeb  = ".deb"
	extUdeb = ".udeb"
)

// IsDeb reports whether a path names a binary package by extension. Both .deb
// and the udeb used by the Debian installer are accepted.
func IsDeb(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	return ext == extDeb || ext == extUdeb
}

// IsDSC reports whether a path names a source control file.
func IsDSC(p string) bool { return strings.EqualFold(path.Ext(p), ".dsc") }

// PackageFromFile reads a .deb and returns the Packages-index stanza describing
// it, with Filename set to the pool location the package belongs at.
//
// The file is read twice: once to pull the control stanza out of the control
// tarball, and once to hash it. Hashing dominates, and doing it separately
// keeps the control extraction from having to buffer the whole package.
func PackageFromFile(localPath string, opts Options) (*aptdata.Package, error) {
	control, err := ControlFromDeb(localPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", localPath, err)
	}

	pkg := aptdata.NewPackage(control)
	pkg.Component = opts.Component
	if pkg.Name() == "" {
		return nil, fmt.Errorf("%s: control file has no Package field", localPath)
	}
	if pkg.Version() == "" {
		return nil, fmt.Errorf("%s: control file has no Version field", localPath)
	}
	if _, err := aptdata.ParseVersion(pkg.Version()); err != nil {
		return nil, fmt.Errorf("%s: %w", localPath, err)
	}
	if pkg.Arch() == "" {
		return nil, fmt.Errorf("%s: control file has no Architecture field", localPath)
	}

	f, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hashes, size, err := aptdata.HashReader(f, opts.hashes())
	if err != nil {
		return nil, fmt.Errorf("hash %s: %w", localPath, err)
	}
	aptdata.SetChecksums(pkg, size, hashes)

	pkg.SetLocation(poolLocation(pkg, localPath, opts))
	aptdata.CanonicalizePackage(pkg)
	return pkg, nil
}

// poolLocation decides where a package file belongs in the repository.
//
// The filename is the conventional name_version_arch.deb rather than whatever
// the local file happens to be called, so a package built into a
// differently-named artifact still lands under a name apt and a human both
// recognize, and so re-adding the same build resolves to the same path.
func poolLocation(pkg *aptdata.Package, localPath string, opts Options) string {
	ext := strings.ToLower(path.Ext(localPath))
	if ext != extDeb && ext != extUdeb {
		ext = extDeb
	}
	// The epoch is not part of a .deb's filename: it is metadata, and a colon
	// is not a portable filename character.
	v := pkg.ParsedVersion()
	v.Epoch = 0
	filename := fmt.Sprintf("%s_%s_%s%s", pkg.Name(), v.String(), pkg.Arch(), ext)

	component := opts.Component
	if component == "" {
		component = "main"
	}
	if !opts.PoolLayout {
		return path.Join(aptdata.PoolDir, component, filename)
	}
	return aptdata.PoolPath(component, pkg.SourceName(), filename)
}

// ControlFromDeb extracts and parses the control stanza from a .deb's control
// tarball.
func ControlFromDeb(localPath string) (*aptdata.Paragraph, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return controlFromReader(f)
}

// controlFromReader walks the ar members looking for the control tarball, then
// walks that tarball looking for ./control.
func controlFromReader(r io.Reader) (*aptdata.Paragraph, error) {
	ar, err := newArReader(r)
	if err != nil {
		return nil, err
	}
	for {
		member, err := ar.Next()
		if errors.Is(err, errArEnd) {
			return nil, fmt.Errorf("no control archive found (not a Debian package?)")
		}
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(member.Name, "control.tar") {
			continue
		}
		decoded, err := aptdata.DecompressStream(member.Name, ar)
		if err != nil {
			return nil, fmt.Errorf("decompress %s: %w", member.Name, err)
		}
		defer decoded.Close()
		return controlFromTar(decoded)
	}
}

// controlFromTar finds and parses ./control inside the control tarball.
func controlFromTar(r io.Reader) (*aptdata.Paragraph, error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("control archive contains no control file")
		}
		if err != nil {
			return nil, fmt.Errorf("read control archive: %w", err)
		}
		if path.Clean(strings.TrimPrefix(hdr.Name, "./")) != "control" {
			continue
		}
		// A control file is a few kilobytes; reading it whole is fine, but cap
		// it so a malformed archive cannot exhaust memory.
		data, err := io.ReadAll(io.LimitReader(tr, 8<<20))
		if err != nil {
			return nil, fmt.Errorf("read control file: %w", err)
		}
		paras, err := aptdata.ParseParagraphs(data)
		if err != nil {
			return nil, fmt.Errorf("parse control file: %w", err)
		}
		if len(paras) == 0 {
			return nil, fmt.Errorf("control file is empty")
		}
		return paras[0], nil
	}
}
