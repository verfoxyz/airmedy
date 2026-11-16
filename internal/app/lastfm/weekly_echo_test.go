package lastfm

import (
	"context"
	"testing"
	"time"

	"airmedy/internal/domain"
)

func fixedClock(unix int64) func() time.Time {
	return func() time.Time { return time.Unix(unix, 0).UTC() }
}

func TestWeeklyEchoBuildsLatestCompletedWeek(t *testing.T) {
	source := connectedSource(nil)
	source.weeks = []LastFmWeek{
		{From: 100, To: 200},
		{From: 200, To: 300},
		{From: 300, To: 400},
	}
	source.weeklyCharts[200] = []LastFmTrack{{
		Artist: "Artist", Title: "Echo", Duration: 200, PlayCount: 12,
	}}
	recommender, _, _ := newStoredRecommender(source, sourceMatcher())
	recommender.now = fixedClock(350)

	collection, err := recommender.ListRecommendationLists(context.Background())
	if err != nil {
		t.Fatalf("ListRecommendationLists: %v", err)
	}
	weekly, ok := summaryByBaseDomain(collection.Lists, domain.LastFmRecommendationWeeklyEcho)
	if !ok {
		t.Fatalf("cards = %+v, want a weekly echo card", collection.Lists)
	}
	if weekly.TrackCount != 1 || weekly.WeekFrom != 200 || weekly.WeekTo != 300 {
		t.Fatalf("weekly card = %+v, want the latest completed week", weekly)
	}
	if source.weeklyChartCalls != 1 {
		t.Fatalf("weekly chart calls = %d, want 1", source.weeklyChartCalls)
	}

	list, err := recommender.GetRecommendationList(
		context.Background(), domain.LastFmRecommendationWeeklyEcho)
	if err != nil {
		t.Fatalf("GetRecommendationList: %v", err)
	}
	if list.WeekFrom != 200 || list.WeekTo != 300 || list.PrevFrom != 100 || list.NextFrom != 0 {
		t.Fatalf("weekly navigation = %+v, want window 200-300 with prev 100", list)
	}
	if len(list.Items) != 1 || list.Items[0].PlayCount != 12 {
		t.Fatalf("weekly items = %+v, want the stored playcount", list.Items)
	}
}

func TestWeeklyEchoWeeksDoNotOverwriteEachOther(t *testing.T) {
	source := connectedSource(nil)
	source.weeks = []LastFmWeek{{From: 100, To: 200}, {From: 200, To: 300}}
	source.weeklyCharts[100] = []LastFmTrack{track("Artist", "Old Week")}
	source.weeklyCharts[200] = []LastFmTrack{track("Artist", "New Week")}
	recommender, _, store := newStoredRecommender(source, sourceMatcher())
	recommender.now = fixedClock(350)

	latest, err := recommender.GetRecommendationList(
		context.Background(), domain.LastFmRecommendationWeeklyEcho)
	if err != nil {
		t.Fatalf("latest week: %v", err)
	}
	if len(latest.Items) != 1 || latest.Items[0].Title != "New Week" {
		t.Fatalf("latest items = %+v, want the newest week", latest.Items)
	}

	older, err := recommender.GetRecommendationList(
		context.Background(), weeklyEchoKind(100))
	if err != nil {
		t.Fatalf("older week: %v", err)
	}
	if len(older.Items) != 1 || older.Items[0].Title != "Old Week" {
		t.Fatalf("older items = %+v, want the stored history", older.Items)
	}
	if older.PrevFrom != 0 || older.NextFrom != 200 {
		t.Fatalf("older navigation = %+v, want next 200 and no prev", older)
	}

	newList, _ := store.GetRecommendationList(context.Background(), weeklyEchoKind(200))
	oldList, _ := store.GetRecommendationList(context.Background(), weeklyEchoKind(100))
	if newList == nil || oldList == nil {
		t.Fatalf("stored weeks = %+v / %+v, want both", newList, oldList)
	}
	if source.weeklyChartCalls != 2 {
		t.Fatalf("weekly chart calls = %d, want one per week", source.weeklyChartCalls)
	}
}

func TestWeeklyEchoReadsAStoredWeekWithoutRequestingItAgain(t *testing.T) {
	source := connectedSource(nil)
	source.weeks = []LastFmWeek{{From: 100, To: 200}}
	source.weeklyCharts[100] = []LastFmTrack{track("Artist", "Stored")}
	recommender, _, _ := newStoredRecommender(source, sourceMatcher())
	recommender.now = fixedClock(250)

	if _, err := recommender.GetRecommendationList(
		context.Background(), domain.LastFmRecommendationWeeklyEcho); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if _, err := recommender.GetRecommendationList(
		context.Background(), domain.LastFmRecommendationWeeklyEcho); err != nil {
		t.Fatalf("second load: %v", err)
	}

	if source.weeklyChartCalls != 1 {
		t.Fatalf("weekly chart calls = %d, want the stored week to be reused", source.weeklyChartCalls)
	}
	if source.weeklyCalls != 1 {
		t.Fatalf("chart list calls = %d, want the 24h cache to answer", source.weeklyCalls)
	}
}

func TestWeeklyEchoWithoutHistoryIsEmpty(t *testing.T) {
	source := connectedSource(nil)
	recommender, _, _ := newStoredRecommender(source, sourceMatcher())
	recommender.now = fixedClock(250)

	collection, err := recommender.ListRecommendationLists(context.Background())
	if err != nil {
		t.Fatalf("ListRecommendationLists: %v", err)
	}
	if _, ok := summaryByBaseDomain(collection.Lists, domain.LastFmRecommendationWeeklyEcho); ok {
		t.Fatalf("cards = %+v, want no weekly card without history", collection.Lists)
	}

	list, err := recommender.GetRecommendationList(
		context.Background(), domain.LastFmRecommendationWeeklyEcho)
	if err != nil {
		t.Fatalf("GetRecommendationList: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("items = %+v, want the empty state", list.Items)
	}
}

func TestWeeklyEchoOnlyChecksTheNewWeekWhenAutoRefreshIsOn(t *testing.T) {
	source := connectedSource(nil)
	source.weeks = []LastFmWeek{{From: 100, To: 200}}
	source.weeklyCharts[100] = []LastFmTrack{track("Artist", "First")}
	recommender, _, _ := newStoredRecommender(source, sourceMatcher())
	recommender.now = fixedClock(250)

	if _, err := recommender.ListRecommendationListsWithRefresh(context.Background(), true); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if source.weeklyChartCalls != 1 {
		t.Fatalf("weekly chart calls = %d, want the first week", source.weeklyChartCalls)
	}

	source.weeks = append(source.weeks, LastFmWeek{From: 200, To: 300})
	source.weeklyCharts[200] = []LastFmTrack{track("Artist", "Second")}
	recommender.now = fixedClock(350)

	// A newer week exists, but the toggle is off: nothing is fetched.
	if _, err := recommender.ListRecommendationListsWithRefresh(context.Background(), false); err != nil {
		t.Fatalf("auto refresh off: %v", err)
	}
	if source.weeklyChartCalls != 1 {
		t.Fatalf("weekly chart calls = %d with auto refresh off, want no check", source.weeklyChartCalls)
	}

	// The daily cache is forced, as it would be after 24 hours.
	if _, err := recommender.weeklyWeeks(context.Background(), true); err != nil {
		t.Fatalf("refresh chart list: %v", err)
	}
	collection, err := recommender.ListRecommendationListsWithRefresh(context.Background(), true)
	if err != nil {
		t.Fatalf("auto refresh on: %v", err)
	}
	weekly, ok := summaryByBaseDomain(collection.Lists, domain.LastFmRecommendationWeeklyEcho)
	if !ok {
		t.Fatalf("cards = %+v, want the weekly card", collection.Lists)
	}
	if weekly.WeekFrom != 200 || weekly.TrackCount != 1 {
		t.Fatalf("weekly card = %+v, want the new week", weekly)
	}
	if source.weeklyChartCalls != 2 {
		t.Fatalf("weekly chart calls = %d, want a build for the new week", source.weeklyChartCalls)
	}
}

func summaryByBaseDomain(
	summaries []RecommendationListSummary, kind string,
) (RecommendationListSummary, bool) {
	for _, summary := range summaries {
		if recommendationBaseKind(summary.Kind) == kind {
			return summary, true
		}
	}
	return RecommendationListSummary{}, false
}
