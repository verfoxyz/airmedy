package lastfm

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"airmedy/internal/domain"
)

const (
	// weeklyChartCacheTTL is how long the week list is reused. It changes when a
	// week closes, so a daily check is enough.
	weeklyChartCacheTTL = 24 * time.Hour
	// weeklyEchoItemLimit bounds one week's rendered rows.
	weeklyEchoItemLimit = 50
)

// cachedWeeklyChart is the user's chart window list, cached per account.
type cachedWeeklyChart struct {
	username  string
	weeks     []LastFmWeek
	expiresAt time.Time
}

// weeklyEchoKind encodes the week into the persisted list kind, so browsing
// history never has to overwrite a prepared week.
func weeklyEchoKind(from int64) string {
	return domain.LastFmRecommendationWeeklyEcho + ":" + strconv.FormatInt(from, 10)
}

// weeklyEchoFrom returns the week start encoded in a persisted kind.
func weeklyEchoFrom(kind string) (int64, bool) {
	prefix := domain.LastFmRecommendationWeeklyEcho + ":"
	if !strings.HasPrefix(kind, prefix) {
		return 0, false
	}
	from, err := strconv.ParseInt(strings.TrimPrefix(kind, prefix), 10, 64)
	if err != nil || from <= 0 {
		return 0, false
	}
	return from, true
}

// recommendationBaseKind strips a week suffix: a detail page opened as
// "weekly_echo" still owns rows persisted under "weekly_echo:<from>".
func recommendationBaseKind(kind string) string {
	if _, ok := weeklyEchoFrom(kind); ok {
		return domain.LastFmRecommendationWeeklyEcho
	}
	return kind
}

func isWeeklyEchoKind(kind string) bool {
	return recommendationBaseKind(kind) == domain.LastFmRecommendationWeeklyEcho
}

// weeklyWeeks returns the chart windows for the connected user.
//
// force skips the 24-hour cache. A request failure is not cached, so a page
// opened while Last.fm is down retries next time.
func (r *Recommender) weeklyWeeks(ctx context.Context, force bool) ([]LastFmWeek, error) {
	_, username, _ := r.client.GetStatus()

	r.mu.Lock()
	cached := r.weeklyChart
	r.mu.Unlock()
	if !force && cached.username == username && !cached.expiresAt.IsZero() &&
		r.now().Before(cached.expiresAt) {
		return append([]LastFmWeek{}, cached.weeks...), nil
	}

	weeks, err := r.client.GetWeeklyChartList(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(weeks, func(i, j int) bool { return weeks[i].From < weeks[j].From })

	r.mu.Lock()
	r.weeklyChart = cachedWeeklyChart{
		username:  username,
		weeks:     append([]LastFmWeek{}, weeks...),
		expiresAt: r.now().Add(weeklyChartCacheTTL),
	}
	r.mu.Unlock()
	return weeks, nil
}

// completedWeeks drops the ongoing window: its chart is still changing, so it
// must not be offered by "next week" or built as the latest week.
func completedWeeks(weeks []LastFmWeek, now time.Time) []LastFmWeek {
	unix := now.Unix()
	result := make([]LastFmWeek, 0, len(weeks))
	for _, week := range weeks {
		if week.To <= unix {
			result = append(result, week)
		}
	}
	return result
}

// latestCompletedWeek is the newest window that has already ended. Last.fm's
// list includes the current, partial week; a chart of it would keep changing.
func latestCompletedWeek(weeks []LastFmWeek, now time.Time) (LastFmWeek, bool) {
	unix := now.Unix()
	for index := len(weeks) - 1; index >= 0; index-- {
		if weeks[index].To <= unix {
			return weeks[index], true
		}
	}
	return LastFmWeek{}, false
}

func findWeek(weeks []LastFmWeek, from int64) (LastFmWeek, bool) {
	for _, week := range weeks {
		if week.From == from {
			return week, true
		}
	}
	return LastFmWeek{}, false
}

// weekNeighbours returns the adjacent windows, or 0 when the week is the
// oldest/newest one Last.fm has.
func weekNeighbours(weeks []LastFmWeek, from int64) (prev, next int64) {
	for index, week := range weeks {
		if week.From != from {
			continue
		}
		if index > 0 {
			prev = weeks[index-1].From
		}
		if index+1 < len(weeks) {
			next = weeks[index+1].From
		}
		return prev, next
	}
	return 0, 0
}

// getWeeklyEchoList returns one week's list, reading the stored rows when the
// week was prepared before and asking Last.fm only for a new week.
func (r *Recommender) getWeeklyEchoList(ctx context.Context, from int64) (RecommendationList, error) {
	empty := RecommendationList{
		Kind:  domain.LastFmRecommendationWeeklyEcho,
		Title: recommendationTitle(domain.LastFmRecommendationWeeklyEcho),
		Items: []TopTrackItem{},
	}
	weeks, err := r.weeklyWeeks(ctx, false)
	if err != nil {
		return empty, err
	}
	if len(weeks) == 0 {
		return empty, nil
	}

	target := from
	completed := completedWeeks(weeks, r.now())
	if target == 0 {
		latest, ok := latestCompletedWeek(completed, r.now())
		if !ok {
			return empty, nil
		}
		target = latest.From
	}
	week, ok := findWeek(weeks, target)
	if !ok {
		return empty, fmt.Errorf("lastfm weekly echo: unknown week %d", target)
	}

	kind := weeklyEchoKind(week.From)
	prev, next := weekNeighbours(completed, week.From)
	if store := r.recommendationStoreOrNil(); store != nil {
		list, err := store.GetRecommendationList(ctx, kind)
		if err != nil {
			return empty, err
		}
		if list != nil {
			result := recommendationListFromDomain(*list)
			result.WeekFrom, result.WeekTo = week.From, week.To
			result.PrevFrom, result.NextFrom = prev, next
			r.resumeRecommendationMatching(ctx, kind)
			return result, nil
		}
	}

	items, err := r.buildWeeklyEcho(ctx, week)
	if err != nil {
		return empty, err
	}
	result := RecommendationList{
		Kind:      kind,
		Title:     recommendationTitle(domain.LastFmRecommendationWeeklyEcho),
		Items:     items,
		UpdatedAt: r.now().UTC(),
		WeekFrom:  week.From,
		WeekTo:    week.To,
		PrevFrom:  prev,
		NextFrom:  next,
	}
	return result, nil
}

// buildWeeklyEcho fetches one window, stores its rows and starts matching.
func (r *Recommender) buildWeeklyEcho(ctx context.Context, week LastFmWeek) ([]TopTrackItem, error) {
	tracks, err := r.client.GetWeeklyTrackChart(ctx, week.From, week.To)
	if err != nil {
		return nil, err
	}
	kind := weeklyEchoKind(week.From)
	items := r.filterDislikedItems(ctx, weeklyEchoItems(tracks, kind, week))
	items = r.inheritStoredMatches(ctx, kind, items)
	if err := r.saveRecommendationList(ctx, kind, items); err != nil {
		return nil, err
	}
	r.startKindMatching(ctx, kind, items)
	return items, nil
}

// weeklyEchoItems keeps every usable row once, with its playcount and window.
func weeklyEchoItems(tracks []LastFmTrack, kind string, week LastFmWeek) []TopTrackItem {
	items := make([]TopTrackItem, 0, len(tracks))
	seen := make(map[string]struct{}, len(tracks))
	for _, track := range tracks {
		if !usableTrack(track) {
			continue
		}
		key := candidateKey(track.Artist, track.Title)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, TopTrackItem{
			Title:     track.Title,
			Artist:    track.Artist,
			Album:     track.Album,
			Duration:  track.Duration,
			Artists:   []string{track.Artist},
			CoverURL:  track.ImageURL,
			Kind:      kind,
			Matching:  true,
			MatchKey:  key,
			PlayCount: track.PlayCount,
			WeekFrom:  week.From,
			WeekTo:    week.To,
		})
		if len(items) >= weeklyEchoItemLimit {
			break
		}
	}
	return items
}

// latestWeeklyEchoSummary returns the newest stored weekly list, if any.
func latestWeeklyEchoSummary(
	summaries []domain.RecommendationListSummary,
) (domain.RecommendationListSummary, bool) {
	var latest domain.RecommendationListSummary
	found := false
	for _, summary := range summaries {
		if !isWeeklyEchoKind(summary.Kind) {
			continue
		}
		if !found || summary.WeekFrom > latest.WeekFrom {
			latest = summary
			found = true
		}
	}
	return latest, found
}

// weeklyEchoAdvanced reports whether Last.fm has a newer completed week than the
// stored one. It spends one chart-list request at most once per day.
func (r *Recommender) weeklyEchoAdvanced(
	ctx context.Context, latest domain.RecommendationListSummary,
) bool {
	weeks, err := r.weeklyWeeks(ctx, false)
	if err != nil {
		r.logger.Debug("lastfm weekly echo: chart list failed", "error", err)
		return false
	}
	week, ok := latestCompletedWeek(weeks, r.now())
	if !ok {
		return false
	}
	return week.From > latest.WeekFrom
}

// RefreshWeeklyEcho rebuilds one week; zero means the latest completed week.
func (r *Recommender) RefreshWeeklyEcho(ctx context.Context, from int64) error {
	connected, username, _ := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		return nil
	}
	weeks, err := r.weeklyWeeks(ctx, true)
	if err != nil {
		return err
	}
	target := from
	if target == 0 {
		latest, ok := latestCompletedWeek(weeks, r.now())
		if !ok {
			return nil
		}
		target = latest.From
	}
	week, ok := findWeek(weeks, target)
	if !ok {
		return fmt.Errorf("lastfm weekly echo: unknown week %d", target)
	}
	_, err = r.buildWeeklyEcho(ctx, week)
	return err
}
