package debmeta

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/danudey/createapt-go/pkg/aptdata"
)

// SourceFiles pairs a source package's index stanza with the local files that
// must be uploaded alongside it: the .dsc itself and every tarball it names.
type SourceFiles struct {
	// Source is the Sources-index stanza.
	Source *aptdata.Source
	// Files maps each file's repo-root-relative destination to its local path.
	Files map[string]string
}

// SourceFromFile reads a .dsc and returns the Sources-index stanza describing
// it, together with the local files it references.
//
// Every file the .dsc names must be present next to it, because a source
// package that is missing its tarball is not a source package — apt would
// index something it cannot fetch. Each file is hashed from its contents and
// checked against the .dsc's own record, so a truncated or substituted tarball
// is caught here rather than by a user's `apt source` months later.
func SourceFromFile(localPath string, opts Options) (*SourceFiles, error) {
	raw, err := os.ReadFile(localPath)
	if err != nil {
		return nil, err
	}
	paras, err := aptdata.ParseParagraphs(StripClearsign(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: parse .dsc: %w", localPath, err)
	}
	if len(paras) == 0 {
		return nil, fmt.Errorf("%s: .dsc is empty", localPath)
	}
	para := paras[0]

	name := para.Get(aptdata.FieldSource)
	if name == "" {
		return nil, fmt.Errorf("%s: .dsc has no Source field", localPath)
	}
	version := para.Get(aptdata.FieldVersion)
	if version == "" {
		return nil, fmt.Errorf("%s: .dsc has no Version field", localPath)
	}
	if _, err := aptdata.ParseVersion(version); err != nil {
		return nil, fmt.Errorf("%s: %w", localPath, err)
	}

	// In a Sources index the source's name lives in Package, not Source.
	para.Set(aptdata.FieldPackage, name)
	para.Delete(aptdata.FieldSource)

	src := aptdata.NewSource(para)
	src.Component = opts.Component

	// The files the .dsc declares, as recorded in it.
	declared := src.SourceFiles()
	if len(declared) == 0 {
		return nil, fmt.Errorf("%s: .dsc lists no files", localPath)
	}

	component := opts.Component
	if component == "" {
		component = "main"
	}
	dir := aptdata.PoolDirFor(component, name)
	if !opts.PoolLayout {
		dir = path.Join(aptdata.PoolDir, component)
	}
	src.SetDirectory(dir)

	algos := opts.hashes()
	localDir := filepath.Dir(localPath)
	out := &SourceFiles{Source: src, Files: map[string]string{}}

	resolved := make([]aptdata.SourceFile, 0, len(declared)+1)
	for _, want := range declared {
		if strings.ContainsAny(want.Name, "/\\") {
			return nil, fmt.Errorf("%s: .dsc names a file with a path separator (%q)", localPath, want.Name)
		}
		companion := filepath.Join(localDir, want.Name)
		got, err := hashLocalFile(companion, algos)
		if err != nil {
			return nil, fmt.Errorf("%s: file %s referenced by the .dsc: %w", localPath, want.Name, err)
		}
		if err := checkDeclared(want, got); err != nil {
			return nil, fmt.Errorf("%s: %w", localPath, err)
		}
		resolved = append(resolved, got)
		out.Files[path.Join(dir, got.Name)] = companion
	}

	// The .dsc itself is part of the source package and is listed alongside the
	// files it names; its own checksums are what identify the source package.
	dscEntry, err := hashLocalFile(localPath, algos)
	if err != nil {
		return nil, err
	}
	dscEntry.Name = fmt.Sprintf("%s_%s.dsc", name, versionWithoutEpoch(version))
	resolved = append(resolved, dscEntry)
	out.Files[path.Join(dir, dscEntry.Name)] = localPath

	src.SetSourceFiles(resolved)
	aptdata.CanonicalizeSource(src)
	return out, nil
}

// checkDeclared compares the hashes a .dsc records for a file against the ones
// computed from the file on disk. Only algorithms the .dsc actually recorded
// are compared, and size is always compared.
func checkDeclared(want, got aptdata.SourceFile) error {
	if want.Size != 0 && want.Size != got.Size {
		return fmt.Errorf("file %s is %d bytes but the .dsc records %d", got.Name, got.Size, want.Size)
	}
	for _, pair := range []struct {
		algo       string
		want, have string
	}{
		{aptdata.HashMD5, want.MD5, got.MD5},
		{aptdata.HashSHA1, want.SHA1, got.SHA1},
		{aptdata.HashSHA256, want.SHA256, got.SHA256},
	} {
		if pair.want == "" || pair.have == "" {
			continue
		}
		if !strings.EqualFold(pair.want, pair.have) {
			return fmt.Errorf("file %s has %s %s but the .dsc records %s",
				got.Name, pair.algo, pair.have, pair.want)
		}
	}
	return nil
}

// hashLocalFile computes a file's size and digests.
func hashLocalFile(p string, algos []string) (aptdata.SourceFile, error) {
	f, err := os.Open(p)
	if err != nil {
		return aptdata.SourceFile{}, err
	}
	defer f.Close()
	hashes, size, err := aptdata.HashReader(f, algos)
	if err != nil {
		return aptdata.SourceFile{}, err
	}
	return aptdata.SourceFile{
		Name:   filepath.Base(p),
		Size:   size,
		MD5:    hashes[aptdata.HashMD5],
		SHA1:   hashes[aptdata.HashSHA1],
		SHA256: hashes[aptdata.HashSHA256],
	}, nil
}

// versionWithoutEpoch renders a version as it appears in a filename, where the
// epoch is omitted because a colon is not a portable filename character.
func versionWithoutEpoch(version string) string {
	v := aptdata.MustParseVersion(version)
	v.Epoch = 0
	return v.String()
}

// StripClearsign returns the body of an OpenPGP clearsigned document, or the
// input unchanged when it is not clearsigned.
//
// A .dsc and an InRelease are both clearsigned, so this is used for reading
// either. Dash-escaped lines ("- " prefixes) are unescaped, which is what makes
// the result the bytes that were actually signed.
func StripClearsign(data []byte) []byte {
	const (
		beginMsg = "-----BEGIN PGP SIGNED MESSAGE-----"
		beginSig = "-----BEGIN PGP SIGNATURE-----"
	)
	if !bytes.Contains(data, []byte(beginMsg)) {
		return data
	}

	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	// States: before the header, inside the armor headers, inside the body.
	const (
		stateBefore = iota
		stateHeaders
		stateBody
	)
	state := stateBefore
	for sc.Scan() {
		line := sc.Text()
		switch state {
		case stateBefore:
			if strings.HasPrefix(line, beginMsg) {
				state = stateHeaders
			}
		case stateHeaders:
			// Armor headers (Hash: ...) run until the first blank line.
			if strings.TrimSpace(line) == "" {
				state = stateBody
			}
		case stateBody:
			if strings.HasPrefix(line, beginSig) {
				return out.Bytes()
			}
			out.WriteString(strings.TrimPrefix(line, "- "))
			out.WriteString("\n")
		}
	}
	if state == stateBody {
		return out.Bytes()
	}
	return data
}
