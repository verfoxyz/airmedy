package domain

import "strings"

// RemotePathPrefix marks a track whose audio is not on disk. Such a path is only
// ever produced by RemotePath; nothing else may construct one.
const RemotePathPrefix = "remote://"

// Track availability states. Only AvailabilityReady guarantees a real file on
// disk; every other state means Path is a virtual remote:// path.
const (
	AvailabilityReady       = "ready"       // audio file exists locally
	AvailabilityRemote      = "remote"      // known online, not downloaded yet
	AvailabilityDownloading = "downloading" // a download job is in flight
	AvailabilityFailed      = "failed"      // last download attempt failed
)

// RemotePath builds the virtual path for an online track. It is the single
// writer of RemotePathPrefix: provider is lowercased and trimmed, externalID is
// only trimmed (some platforms use case-sensitive ids). Callers must not build
// "remote://" strings by hand, otherwise UNIQUE(path) stops deduplicating.
func RemotePath(provider, externalID string) string {
	return RemotePathPrefix + NormalizeProvider(provider) + "/" + strings.TrimSpace(externalID)
}

// NormalizeProvider is the canonical form stored in tracks.provider. Every
// lookup and every RemotePath call goes through it, so "WY" and " wy " can never
// produce two rows for the same identity.
func NormalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

// RemoteTrackSpec is the metadata needed to register an online track in the
// library before its audio exists. Only identity (Provider + ExternalID) is
// required; the rest fills the row so lists and search can show the track.
type RemoteTrackSpec struct {
	Provider   string
	ExternalID string
	Title      string
	Artists    []string
	AlbumName  string
	Duration   int // seconds; 0 when the source does not know it
	CoverURL   string
}

// IsRemote reports whether the track's audio is not currently on disk. A remote
// track that finished downloading becomes AvailabilityReady with a real path, so
// it is no longer remote. Pre-existing rows keep the 'ready' default.
func (t *Track) IsRemote() bool {
	if t == nil {
		return false
	}
	if t.Availability != "" && t.Availability != AvailabilityReady {
		return true
	}
	return strings.HasPrefix(t.Path, RemotePathPrefix)
}
