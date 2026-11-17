package lastfm

import "testing"

func diverseTrack(artist, title, album string) LastFmTrack {
	return LastFmTrack{Artist: artist, Title: title, Album: album}
}

func TestSelectDiverseTracksCapsArtistAndAlbum(t *testing.T) {
	candidates := []LastFmTrack{
		diverseTrack("Artist A", "One", "Album X"),
		diverseTrack("Artist B", "Two", "Album X"),
		diverseTrack("Artist A", "Three", "Album Y"),
		diverseTrack("Artist A", "Four", "Album Z"),
		diverseTrack("Artist C", "Five", "Album W"),
	}
	got := selectDiverseTracks(candidates, 5, nil)
	artists := make(map[string]int)
	albums := make(map[string]int)
	for _, track := range got {
		artists[track.Artist]++
		if track.Album != "" {
			albums[track.Artist+"\x00"+track.Album]++
		}
	}
	if artists["Artist A"] > diversityArtistLimit {
		t.Fatalf("Artist A rows = %d, want at most %d", artists["Artist A"], diversityArtistLimit)
	}
	if albums["Artist A\x00Album X"] != 1 {
		t.Fatalf("Artist A / Album X = %d, want one row per album", albums["Artist A\x00Album X"])
	}
	// One row stays out: Three and Four both belong to Artist A after the
	// earlier adjacency relaxation, and the per-artist cap is hard.
	if len(got) != 4 {
		t.Fatalf("selected = %d rows, want 4 (%v)", len(got), trackTitles(got))
	}
}

func TestSelectDiverseTracksRelaxesAlbumBeforeAdjacency(t *testing.T) {
	candidates := []LastFmTrack{
		diverseTrack("Artist A", "One", "Album X"),
		diverseTrack("Artist B", "Two", "Album X"),
	}
	got := selectDiverseTracks(candidates, 2, nil)
	if len(got) != 2 {
		t.Fatalf("selected = %v, want both rows after the album rule relaxes", trackTitles(got))
	}
	if got[0].Album != got[1].Album {
		t.Fatalf("selected = %v, want the shared album allowed by relaxation", trackTitles(got))
	}
}

func TestSelectDiverseTracksKeepsArtistsAlternating(t *testing.T) {
	candidates := []LastFmTrack{
		diverseTrack("Artist A", "One", "Album 1"),
		diverseTrack("Artist A", "Two", "Album 2"),
		diverseTrack("Artist B", "Three", "Album 3"),
		diverseTrack("Artist B", "Four", "Album 4"),
		diverseTrack("Artist C", "Five", "Album 5"),
	}
	got := selectDiverseTracks(candidates, 5, nil)
	if len(got) != 5 {
		t.Fatalf("selected = %v, want every row", trackTitles(got))
	}
	for index := 1; index < len(got); index++ {
		if got[index-1].Artist == got[index].Artist {
			t.Fatalf("adjacent rows share %q: %v", got[index].Artist, trackTitles(got))
		}
	}
	opening := make(map[string]struct{})
	for index, track := range got {
		if index >= 10 {
			break
		}
		opening[track.Artist] = struct{}{}
	}
	if len(opening) < 3 {
		t.Fatalf("opening artists = %v, want at least three", opening)
	}
}

func TestSelectDiverseTracksKeepsTheArtistCapHard(t *testing.T) {
	candidates := []LastFmTrack{
		diverseTrack("Artist A", "One", ""),
		diverseTrack("Artist A", "Two", ""),
		diverseTrack("Artist A", "Three", ""),
		diverseTrack("Artist B", "Four", ""),
	}
	got := selectDiverseTracks(candidates, 10, nil)
	count := 0
	for _, track := range got {
		if track.Artist == "Artist A" {
			count++
		}
	}
	if count > diversityArtistLimit {
		t.Fatalf("Artist A rows = %d, want at most %d (%v)",
			count, diversityArtistLimit, trackTitles(got))
	}
}

func TestSelectDiverseTracksHonoursInitialArtistCounts(t *testing.T) {
	candidates := []LastFmTrack{
		diverseTrack("Artist A", "One", ""),
		diverseTrack("Artist B", "Two", ""),
	}
	got := selectDiverseTracks(candidates, 2, map[string]int{"artist a": diversityArtistLimit})
	if len(got) != 1 || got[0].Artist != "Artist B" {
		t.Fatalf("selected = %v, want only the artist with room left", trackTitles(got))
	}
}

func trackTitles(tracks []LastFmTrack) []string {
	result := make([]string, 0, len(tracks))
	for _, track := range tracks {
		result = append(result, track.Title)
	}
	return result
}
