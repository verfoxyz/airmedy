package domain

// RemoteTrackSaved is the in-process notification that a remote row joined the
// library through a user action (search save, favorite, playlist add).
//
// Emitters do not decide anything: they register the row, add membership and
// publish this event. mediarequest.Saver is the single decision point — it
// upserts one media request and the worker promotes a cached copy or downloads.
type RemoteTrackSaved struct {
	Track *TrackDTO
	// PlaylistID is where the row was added, or "" for a favorite/search save.
	PlaylistID string
	// CoverURL is the artwork the source reported, when the caller has one. The
	// coordinator falls back to the cover cache's stored URL.
	CoverURL string
	// Explicit is true when the user asked for this save (favorite or search
	// "save"), which allows a download even when the playlist auto-download
	// setting is off. A playlist hook leaves it false so the setting decides.
	Explicit bool
}
