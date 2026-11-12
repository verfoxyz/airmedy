package catalog

import (
	"context"
	"testing"

	"airmedy/internal/domain"
)

func TestMatchTrackKeepsOnlyTheSameRecording(t *testing.T) {
	sources := fakeSources{"wy": wySource(), "kg": kgSource()}
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {
			Rows: []domain.PluginSearchResultRow{
				row("exact", "晴天", "周杰伦"),
				row("suffix", "晴天 (Live)", "周杰伦"),
				row("other-artist", "晴天", "别人"),
			},
		},
		"kg": {
			Rows: []domain.PluginSearchResultRow{
				row("kg-exact", "晴天", "周杰伦"),
			},
		},
	}}
	svc := newTestService(sources, searcher, fakeResolver{"wy": true, "kg": true}, Config{})

	rows, err := svc.MatchTrack(context.Background(), "晴天", "周杰伦", 0, []string{"wy", "kg"})
	if err != nil {
		t.Fatalf("MatchTrack: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("matches = %d, want one merged exact hit: %+v", len(rows), rows)
	}
	if rows[0].Title != "晴天" {
		t.Fatalf("matched title = %q", rows[0].Title)
	}
	if len(rows[0].Sources) != 2 {
		t.Fatalf("sources = %v, want the row merged across both providers", rows[0].Sources)
	}
}

func TestMatchTrackAppliesDurationTolerance(t *testing.T) {
	sources := fakeSources{"wy": wySource()}
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {
			Rows: []domain.PluginSearchResultRow{
				{ExternalID: "near", Title: "Song", Artists: []string{"Artist"}, Duration: 244},
				{ExternalID: "far", Title: "Song", Artists: []string{"Artist"}, Duration: 246},
			},
		},
	}}
	svc := newTestService(sources, searcher, fakeResolver{"wy": true}, Config{})

	rows, err := svc.MatchTrack(context.Background(), "Song", "Artist", 239, []string{"wy"})
	if err != nil {
		t.Fatalf("MatchTrack: %v", err)
	}
	if len(rows) != 1 || rows[0].ExternalID != "near" {
		t.Fatalf("matches = %+v, want only the row within ±5s", rows)
	}
}

func TestMatchTrackRespectsTheSourceFilter(t *testing.T) {
	sources := fakeSources{"wy": wySource(), "kg": kgSource()}
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {Rows: []domain.PluginSearchResultRow{row("wy-1", "Song", "Artist")}},
		"kg": {Rows: []domain.PluginSearchResultRow{row("kg-1", "Song", "Artist")}},
	}}
	svc := newTestService(sources, searcher, fakeResolver{"wy": true, "kg": true}, Config{})

	rows, err := svc.MatchTrack(context.Background(), "Song", "Artist", 0, []string{"kg"})
	if err != nil {
		t.Fatalf("MatchTrack: %v", err)
	}
	if len(rows) != 1 || rows[0].Provider != "kg" {
		t.Fatalf("matches = %+v, want only the requested source", rows)
	}
}

func TestMatchTrackAcceptsAnyCreditedArtist(t *testing.T) {
	sources := fakeSources{"wy": wySource()}
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {
			Rows: []domain.PluginSearchResultRow{
				{ExternalID: "featured", Title: "Song", Artists: []string{"Primary", "Featured"}},
				{ExternalID: "unrelated", Title: "Song", Artists: []string{"Someone Else"}},
			},
		},
	}}
	svc := newTestService(sources, searcher, fakeResolver{"wy": true}, Config{})

	rows, err := svc.MatchTrack(context.Background(), "Song", "Featured", 0, []string{"wy"})
	if err != nil {
		t.Fatalf("MatchTrack: %v", err)
	}
	if len(rows) != 1 || rows[0].ExternalID != "featured" {
		t.Fatalf("matches = %+v, want the row crediting the wanted artist", rows)
	}
}

// fakeAliasRepo expands one hop for the cloud-matching tests.
type fakeAliasRepo struct{ pairs map[string][]string }

func (f fakeAliasRepo) ListArtistAliases(context.Context) ([]domain.ArtistAlias, error) {
	return nil, nil
}

func (f fakeAliasRepo) AddArtistAlias(context.Context, string, string, string) error { return nil }
func (f fakeAliasRepo) RemoveArtistAlias(context.Context, string, string) error      { return nil }
func (f fakeAliasRepo) SetArtistAliasEnabled(context.Context, string, string, bool) error {
	return nil
}

func (f fakeAliasRepo) ExpandArtists(_ context.Context, keys []string) ([]string, error) {
	out := append([]string{}, keys...)
	for _, key := range keys {
		out = append(out, f.pairs[key]...)
	}
	return out, nil
}

func TestMatchTrackExpandsArtistAliases(t *testing.T) {
	sources := fakeSources{"wy": wySource()}
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {
			Rows: []domain.PluginSearchResultRow{
				{ExternalID: "alias-hit", Title: "crystallized", Artists: []string{"かめりあ"}},
			},
		},
	}}
	svc := newTestService(sources, searcher, fakeResolver{"wy": true}, Config{})
	svc.SetArtistAliases(fakeAliasRepo{pairs: map[string][]string{
		"camellia": {"かめりあ"},
	}})

	rows, err := svc.MatchTrack(context.Background(), "crystallized", "Camellia", 0, []string{"wy"})
	if err != nil {
		t.Fatalf("MatchTrack: %v", err)
	}
	if len(rows) != 1 || rows[0].ExternalID != "alias-hit" {
		t.Fatalf("matches = %+v, want the alias-expanded artist hit", rows)
	}
}

func TestMatchTrackIgnoresArtistOrderAndSeparators(t *testing.T) {
	sources := fakeSources{"wy": wySource()}
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {
			Rows: []domain.PluginSearchResultRow{
				{ExternalID: "reordered", Title: "Synthetic Fracture",
					Artists: []string{"Gardens, Sad Keyboard Guy"}},
			},
		},
	}}
	svc := newTestService(sources, searcher, fakeResolver{"wy": true}, Config{})

	rows, err := svc.MatchTrack(context.Background(), "Synthetic Fracture",
		"Sad Keyboard Guy;Gardens", 0, []string{"wy"})
	if err != nil {
		t.Fatalf("MatchTrack: %v", err)
	}
	if len(rows) != 1 || rows[0].ExternalID != "reordered" {
		t.Fatalf("matches = %+v, want the reordered credit to match", rows)
	}
}

func TestMatchTrackIgnoresBlankIdentity(t *testing.T) {
	svc := newTestService(fakeSources{"wy": wySource()}, &fakeSearcher{
		results: map[string]domain.PluginSearchResult{
			"wy": {Rows: []domain.PluginSearchResultRow{row("1", "Song", "Artist")}},
		},
	}, fakeResolver{"wy": true}, Config{})

	rows, err := svc.MatchTrack(context.Background(), "  ", "Artist", 0, nil)
	if err != nil {
		t.Fatalf("MatchTrack: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("matches = %d, want none for a blank title", len(rows))
	}
}
