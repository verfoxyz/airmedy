package domain

import (
	"context"
	"errors"
	"time"
)

// Artist alias evidence. A "merge" alias was learned from a successful merge of
// the same song; a "manual" one was entered in settings.
const (
	ArtistAliasEvidenceMerge  = "merge"
	ArtistAliasEvidenceManual = "manual"
)

var (
	// ErrArtistAliasInvalid means one side normalized to an empty key.
	ErrArtistAliasInvalid = errors.New("artist alias: both names are required")
	// ErrArtistAliasSelf means the two sides normalize to the same key.
	ErrArtistAliasSelf = errors.New("artist alias: a name cannot alias itself")
	// ErrArtistAliasExists means the pair is already known (either direction).
	ErrArtistAliasExists = errors.New("artist alias: this pair already exists")
)

// ArtistAlias is one stored alias pair. Only one direction is stored; expansion
// is bidirectional, so A→B also lets B match A.
type ArtistAlias struct {
	AliasKey     string    `json:"alias_key" db:"alias_key"`
	CanonicalKey string    `json:"canonical_key" db:"canonical_key"`
	Evidence     string    `json:"evidence" db:"evidence"`
	Enabled      bool      `json:"enabled" db:"enabled"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
}

// ArtistAliasRepository persists alias pairs and expands matching keys.
//
// ExpandArtists returns each input key plus its enabled one-hop aliases. It is
// called from every matching path, so implementations must treat a missing table
// value as an empty result, never as an error.
type ArtistAliasRepository interface {
	ListArtistAliases(ctx context.Context) ([]ArtistAlias, error)
	// AddArtistAlias inserts one direction. It returns ErrArtistAliasExists when
	// the pair is already known in either direction.
	AddArtistAlias(ctx context.Context, aliasKey, canonicalKey, evidence string) error
	// RemoveArtistAlias deletes both directions of the pair.
	RemoveArtistAlias(ctx context.Context, aliasKey, canonicalKey string) error
	// SetArtistAliasEnabled flips both directions of the pair.
	SetArtistAliasEnabled(ctx context.Context, aliasKey, canonicalKey string, enabled bool) error
	// ExpandArtists expands one hop through enabled aliases.
	ExpandArtists(ctx context.Context, keys []string) ([]string, error)
}
