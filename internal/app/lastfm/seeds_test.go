package lastfm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"airmedy/internal/domain"
)

// fakeSeedRepository is the local listening-signal source.
type fakeSeedRepository struct {
	listened []domain.RecommendationSeedTrack
	played   []domain.RecommendationSeedTrack
	playlist []domain.RecommendationSeedTrack
	recent   []domain.RecommendationSeedTrack
	genres   []string
	since    time.Time
}

func (f *fakeSeedRepository) MostListenedTracks(
	_ context.Context, since time.Time, _ int,
) ([]domain.RecommendationSeedTrack, error) {
	f.since = since
	return append([]domain.RecommendationSeedTrack{}, f.listened...), nil
}

func (f *fakeSeedRepository) MostPlayedTracks(
	context.Context, int,
) ([]domain.RecommendationSeedTrack, error) {
	return append([]domain.RecommendationSeedTrack{}, f.played...), nil
}

func (f *fakeSeedRepository) PlaylistTracks(
	context.Context, int,
) ([]domain.RecommendationSeedTrack, error) {
	return append([]domain.RecommendationSeedTrack{}, f.playlist...), nil
}

func (f *fakeSeedRepository) RecentlyListenedTracks(
	_ context.Context, _ time.Time, _ int,
) ([]domain.RecommendationSeedTrack, error) {
	return append([]domain.RecommendationSeedTrack{}, f.recent...), nil
}

func (f *fakeSeedRepository) TopGenres(context.Context, int) ([]string, error) {
	return append([]string{}, f.genres...), nil
}

func localSeedRows(prefix string, count int) []domain.RecommendationSeedTrack {
	rows := make([]domain.RecommendationSeedTrack, 0, count)
	for index := 0; index < count; index++ {
		rows = append(rows, domain.RecommendationSeedTrack{
			TrackID:   fmt.Sprintf("%s-%d", prefix, index),
			Title:     fmt.Sprintf("%s Song %d", prefix, index),
			Artists:   []string{fmt.Sprintf("%s Artist %d", prefix, index)},
			AlbumName: fmt.Sprintf("%s Album %d", prefix, index),
		})
	}
	return rows
}

func TestRecommendationSeedsMergeWeightsAndPreferLocal(t *testing.T) {
	source := connectedSource(nil)
	for index := 0; index < 20; index++ {
		source.top[topTrackPeriod] = append(source.top[topTrackPeriod],
			track(fmt.Sprintf("Last.fm Artist %d", index), fmt.Sprintf("Last.fm Song %d", index)))
	}
	source.topArtists["6month"] = []LastFmArtist{{Name: "Last.fm Artist 0"}}

	recommender := newTestRecommender(source, nil, &fakeStreamRepo{})
	recommender.SetSeedRepository(&fakeSeedRepository{
		listened: localSeedRows("Local", 8),
	})

	set := recommender.weeklyRecommendationSeeds(context.Background(), nil)
	if len(set.tracks) != seedTrackLimit {
		t.Fatalf("tracks = %d, want %d", len(set.tracks), seedTrackLimit)
	}
	local, lastFmOnly := 0, 0
	for _, entry := range set.tracks {
		if entry.local {
			local++
		} else {
			lastFmOnly++
		}
	}
	if local < 6 {
		t.Fatalf("local seeds = %d, want at least 60%%", local)
	}
	if lastFmOnly > int(seedTrackLimit*seedLastFmShare) {
		t.Fatalf("Last.fm-only seeds = %d, want at most 40%%", lastFmOnly)
	}
	if set.general {
		t.Fatal("a local history must not be marked as the general fallback")
	}
}

func TestRecommendationSeedsAreStableWithinTheWeekAndRotateAcrossWeeks(t *testing.T) {
	source := connectedSource(nil)
	repo := &fakeSeedRepository{listened: localSeedRows("Week One", 10)}
	recommender := newTestRecommender(source, nil, &fakeStreamRepo{})
	recommender.SetSeedRepository(repo)
	firstWeek := time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)
	recommender.now = func() time.Time { return firstWeek }

	first := recommender.weeklyRecommendationSeeds(context.Background(), nil)
	// Same week, different local data: the cache must answer unchanged.
	repo.listened = localSeedRows("Ignored", 10)
	again := recommender.weeklyRecommendationSeeds(context.Background(), nil)
	if keysOfSeeds(first.tracks) != keysOfSeeds(again.tracks) {
		t.Fatalf("seeds changed inside one week:\n%v\n%v", keysOfSeeds(first.tracks), keysOfSeeds(again.tracks))
	}

	repo.listened = localSeedRows("Week Two", 10)
	recommender.now = func() time.Time { return firstWeek.AddDate(0, 0, 7) }
	next := recommender.weeklyRecommendationSeeds(context.Background(), nil)
	if len(next.tracks) != seedTrackLimit {
		t.Fatalf("next week tracks = %d, want %d", len(next.tracks), seedTrackLimit)
	}
	overlap := 0
	seen := make(map[string]struct{}, len(first.tracks))
	for _, entry := range first.tracks {
		seen[candidateKey(entry.track.Artist, entry.track.Title)] = struct{}{}
	}
	for _, entry := range next.tracks {
		if _, ok := seen[candidateKey(entry.track.Artist, entry.track.Title)]; ok {
			overlap++
		}
	}
	wantOverlap := seedTrackLimit / 2
	if overlap != wantOverlap {
		t.Fatalf("overlap = %d, want %d from the previous week", overlap, wantOverlap)
	}
}

func TestRecommendationSeedsColdStartUsesChartAndTags(t *testing.T) {
	source := connectedSource(nil)
	source.chartTopTracks = []LastFmTrack{
		track("Chart Artist One", "Chart One"),
		track("Chart Artist Two", "Chart Two"),
	}
	source.tagTopTracks["ambient"] = []LastFmTrack{track("Tag Artist", "Tag Song")}
	recommender := newTestRecommender(source, nil, &fakeStreamRepo{})
	recommender.SetSeedRepository(&fakeSeedRepository{genres: []string{"ambient"}})

	set := recommender.weeklyRecommendationSeeds(context.Background(), nil)
	if !set.general {
		t.Fatal("an empty local library and empty Last.fm seeds must be the general fallback")
	}
	if len(set.tracks) == 0 {
		t.Fatal("the cold-start fallback returned no seeds")
	}
	if source.chartTopTracksCalls == 0 && source.tagTopTracksCalls == 0 {
		t.Fatal("the fallback did not ask Last.fm for chart or tag tracks")
	}
}

func keysOfSeeds(tracks []seedTrack) string {
	result := ""
	for _, entry := range tracks {
		result += candidateKey(entry.track.Artist, entry.track.Title) + "\x1e"
	}
	return result
}
