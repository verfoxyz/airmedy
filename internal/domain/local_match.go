package domain

// LocalMatchSuspiciousSeconds is the duration difference above which a local
// match is still accepted but flagged for the user to confirm. Title and artist
// set decide the match; duration is only a hint because Last.fm and source
// metadata are often tens of seconds apart.
const LocalMatchSuspiciousSeconds = 10

// LocalMatchCandidateWindowSeconds bounds the database scan used to find local
// candidates. It is deliberately wider than the suspicious threshold: a file
// whose duration disagrees by more than LocalMatchSuspiciousSeconds is still
// the same song, and creating a second row is worse than a flagged match.
const LocalMatchCandidateWindowSeconds = 120

// LocalTrackMatch is the local-first answer for one song.
//
// Track is the on-disk row to reuse. Suspicious reports that title and artist
// set agreed but the durations differ by more than
// LocalMatchSuspiciousSeconds, so the UI must ask the user to confirm instead
// of auto-saving or auto-downloading.
type LocalTrackMatch struct {
	Track      *TrackDTO
	Suspicious bool
}

// CatalogLocalMatch is one "this cloud row is already on disk" answer.
type CatalogLocalMatch struct {
	TrackID    string
	Suspicious bool
}
