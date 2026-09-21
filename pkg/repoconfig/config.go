// Package repoconfig persists a small createapt-go configuration file
// alongside a repository (at the repository root, next to dists/ and pool/).
// It records the repository's identity (name and public base URL), the layout
// packages are published in, the suite and component later operations default
// to, and the signing choices that were used, so later operations can default
// to the same settings instead of re-specifying them on every invocation.
//
// The file is plain JSON and is not part of the apt metadata: apt ignores it,
// and it is never referenced by a Release file.
package repoconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/danudey/createapt-go/pkg/backend"
)

// Path is the repo-root-relative location of the configuration file.
const Path = "createapt-go.json"

// Config holds the persisted repository defaults.
type Config struct {
	// Name is a human-readable repository name.
	Name string `json:"name,omitempty"`
	// BaseURL is the URL end users fetch the repository from.
	BaseURL string `json:"baseurl,omitempty"`

	// Suite is the dists/ subdirectory the repository's packages were last
	// published into, and the one a later operation defaults to. Recording it
	// means an `add` that omits --suite keeps updating the same suite instead
	// of silently creating a second one.
	Suite string `json:"suite,omitempty"`
	// Component is the section packages were last placed in, defaulted to for
	// the same reason.
	Component string `json:"component,omitempty"`

	// Target is the compatibility profile (debian12, ubuntu22.04 or an alias)
	// whose defaults were selected, if any. Recording it lets later operations
	// re-apply the profile's current and future defaults (index compression,
	// hash set, and any future target-derived settings) without re-specifying
	// --target.
	Target string `json:"target,omitempty"`

	// PoolLayout records whether packages were placed in the conventional
	// pool/<component>/<prefix>/<source>/ tree, so later uploads keep landing
	// in the same place.
	PoolLayout bool `json:"pool_layout"`

	// ByHash records whether indexes were published under their checksums as
	// well as their plain names.
	ByHash bool `json:"by_hash"`

	// Origin, Label, Codename, Version and Description are the Release file's
	// descriptive fields, preserved across publishes.
	Origin      string `json:"origin,omitempty"`
	Label       string `json:"label,omitempty"`
	Codename    string `json:"codename,omitempty"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`

	// SignRelease records whether the Release file was signed.
	SignRelease bool `json:"sign_release"`
	// GPGKeyID is the primary-key fingerprint of the key used for signing.
	GPGKeyID string `json:"gpg_key_id,omitempty"`

	// CopySource records the repository this one was copied from (the source
	// location given to `copy`). It lets a later `copy --continue` confirm that
	// it is resuming into the same destination rather than overwriting an
	// unrelated repository.
	CopySource string `json:"copy_source,omitempty"`
}

// Load reads the configuration file from the backend. A repository with no
// config file yields (nil, nil) so callers can treat it as "no recorded
// defaults".
func Load(ctx context.Context, be backend.Backend) (*Config, error) {
	rc, err := be.Get(ctx, Path)
	if errors.Is(err, backend.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", Path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Path, err)
	}
	return &cfg, nil
}

// Save writes cfg to the backend as pretty-printed JSON.
func Save(ctx context.Context, be backend.Backend, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return be.Put(ctx, Path, bytes.NewReader(data), int64(len(data)))
}
