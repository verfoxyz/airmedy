package lastfm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"airmedy/internal/domain"
)

// recommendationStoreTimeout bounds one persistence write, so a busy database
// never blocks the matching workers for long.
const recommendationStoreTimeout = 5 * time.Second

// RecommendationListSummary is the card data of one persisted list.
type RecommendationListSummary struct {
	Kind          string    `json:"kind"`
	Title         string    `json:"title"`
	TrackCount    int       `json:"track_count"`
	MatchingCount int       `json:"matching_count"`
	TotalDuration int       `json:"total_duration"`
	CoverURLs     []string  `json:"cover_urls"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// RecommendationList is one list's rows in Last.fm order.
type RecommendationList struct {
	Kind      string         `json:"kind"`
	Title     string         `json:"title"`
	Items     []TopTrackItem `json:"items"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// RecommendationListCollection is the cards page payload. Status is the same
// machine string the old page used: ok or not_connected.
type RecommendationListCollection struct {
	Status string                      `json:"status"`
	Lists  []RecommendationListSummary `json:"lists"`
}

// recommendationKindOrder is the card order: "for you" above the top tracks.
func recommendationKindOrder() []string {
	return []string{domain.LastFmRecommendationSimilar, domain.LastFmRecommendationTopTracks}
}

// recommendationTitle returns the i18n key the page words. The key is stored
// with the list, so a list added later only needs an entry here.
func recommendationTitle(kind string) string {
	switch kind {
	case domain.LastFmRecommendationTopTracks:
		return "lastfm.top"
	case domain.LastFmRecommendationSimilar:
		return "lastfm.similar_tracks"
	default:
		return kind
	}
}

func knownRecommendationKind(kind string) bool {
	return kind == domain.LastFmRecommendationTopTracks ||
		kind == domain.LastFmRecommendationSimilar
}

// recommendationItemsAddedEvent streams rows while the similar list is being
// built, one batch per seed.
const recommendationItemsAddedEvent = "lastfm:recommendation-items-added"

// RecommendationItemsAdded is the payload of lastfm:recommendation-items-added.
type RecommendationItemsAdded struct {
	Kind  string         `json:"kind"`
	Items []TopTrackItem `json:"items"`
}

// ListRecommendationLists returns the stored cards. A list that has never been
// built is built once; afterwards the page reads from the database until the
// user refreshes.
func (r *Recommender) ListRecommendationLists(
	ctx context.Context,
) (RecommendationListCollection, error) {
	empty := RecommendationListCollection{Status: StatusOK, Lists: []RecommendationListSummary{}}
	connected, username, _ := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		return RecommendationListCollection{Status: StatusNotConnected, Lists: []RecommendationListSummary{}}, nil
	}

	store := r.recommendationStoreOrNil()
	if store == nil {
		page, err := r.GetTopTrackRecommendations(ctx)
		if err != nil {
			return empty, err
		}
		return RecommendationListCollection{
			Status: page.Status,
			Lists:  summariesFromItems(page.Items, page.SimilarItems),
		}, nil
	}

	summaries, err := store.ListRecommendationLists(ctx)
	if err != nil {
		return empty, err
	}

	missingTop := !hasRecommendationKind(summaries, domain.LastFmRecommendationTopTracks)
	missingSimilar := !hasRecommendationKind(summaries, domain.LastFmRecommendationSimilar)
	if missingTop || missingSimilar {
		// Build only what is missing; an existing list must survive untouched.
		if buildErr := r.buildStoredLists(ctx, missingTop, missingSimilar); buildErr != nil {
			return RecommendationListCollection{Status: StatusOK, Lists: orderSummaries(summaries)}, buildErr
		}
		summaries, err = store.ListRecommendationLists(ctx)
		if err != nil {
			return empty, err
		}
	}

	r.resumeRecommendationMatching(ctx, "")
	return RecommendationListCollection{Status: StatusOK, Lists: orderSummaries(summaries)}, nil
}

// GetRecommendationList returns one persisted list. A missing kind is built once
// with the same store-then-match flow, so matching never races the insert.
func (r *Recommender) GetRecommendationList(
	ctx context.Context, kind string,
) (RecommendationList, error) {
	if !knownRecommendationKind(kind) {
		return RecommendationList{}, fmt.Errorf("lastfm recommendations: unknown list %q", kind)
	}
	connected, username, _ := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		return RecommendationList{
			Kind:  kind,
			Title: recommendationTitle(kind),
			Items: []TopTrackItem{},
		}, nil
	}

	store := r.recommendationStoreOrNil()
	if store == nil {
		collected, err := r.collectRecommendations(
			ctx, kind == domain.LastFmRecommendationTopTracks, kind == domain.LastFmRecommendationSimilar, false)
		if err != nil {
			return RecommendationList{Kind: kind, Title: recommendationTitle(kind)}, err
		}
		r.startRecommendationMatching(ctx, collected, false)
		items := collected.topItems
		if kind == domain.LastFmRecommendationSimilar {
			items = collected.similarItems
		}
		return RecommendationList{
			Kind:      kind,
			Title:     recommendationTitle(kind),
			Items:     items,
			UpdatedAt: r.now().UTC(),
		}, nil
	}

	list, err := store.GetRecommendationList(ctx, kind)
	if err != nil {
		return RecommendationList{}, err
	}
	if list == nil {
		if buildErr := r.buildStoredLists(
			ctx, kind == domain.LastFmRecommendationTopTracks, kind == domain.LastFmRecommendationSimilar,
		); buildErr != nil {
			return RecommendationList{Kind: kind, Title: recommendationTitle(kind)}, buildErr
		}
		list, err = store.GetRecommendationList(ctx, kind)
		if err != nil {
			return RecommendationList{}, err
		}
		if list == nil {
			return RecommendationList{
				Kind:  kind,
				Title: recommendationTitle(kind),
				Items: []TopTrackItem{},
			}, nil
		}
	}
	r.resumeRecommendationMatching(ctx, kind)
	return recommendationListFromDomain(*list), nil
}

// RefreshRecommendationList rebuilds exactly one list. A failed top-tracks
// request keeps the stored rows and their match state untouched.
func (r *Recommender) RefreshRecommendationList(ctx context.Context, kind string) error {
	if !knownRecommendationKind(kind) {
		return fmt.Errorf("lastfm recommendations: unknown list %q", kind)
	}
	connected, username, _ := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		return nil
	}

	tracks, err := r.client.GetTopTracks(ctx, topTrackPeriod, topTrackLimit)
	if err != nil {
		return err
	}
	if kind == domain.LastFmRecommendationTopTracks {
		return r.storeTopList(ctx, tracks)
	}
	_, err = r.streamSimilarList(ctx, tracks)
	return err
}

// RefreshAllRecommendationLists rebuilds both lists. The top list is written
// first; the similar list then streams in while its rows are matched.
func (r *Recommender) RefreshAllRecommendationLists(ctx context.Context) error {
	connected, username, _ := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		return nil
	}

	tracks, err := r.client.GetTopTracks(ctx, topTrackPeriod, topTrackLimit)
	if err != nil {
		return err
	}
	if err := r.storeTopList(ctx, tracks); err != nil {
		return err
	}
	_, err = r.streamSimilarList(ctx, tracks)
	return err
}

// RefreshRecommendationItem re-runs source matching for one row without asking
// Last.fm for the content again.
//
// The row's cached source results and failed providers are dropped, the row is
// reset to matching, persisted and pushed back through the five-source queue.
// Other rows keep their caches.
func (r *Recommender) RefreshRecommendationItem(
	ctx context.Context, kind, matchKey string,
) error {
	if !knownRecommendationKind(kind) {
		return fmt.Errorf("lastfm recommendations: unknown list %q", kind)
	}
	matchKey = strings.TrimSpace(matchKey)
	if matchKey == "" {
		return fmt.Errorf("lastfm recommendations: empty match key")
	}
	connected, username, _ := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		return nil
	}

	base, ok := r.recommendationBaseRow(ctx, kind, matchKey)
	if !ok {
		return fmt.Errorf("lastfm recommendations: unknown row %q", matchKey)
	}

	rowKey := recommendationRowKey(kind, matchKey)
	r.mu.Lock()
	prefix := rowKey + "|"
	for key := range r.sourceMatches {
		if strings.HasPrefix(key, prefix) {
			delete(r.sourceMatches, key)
		}
	}
	delete(r.topFailedSources, rowKey)
	r.topRows[rowKey] = base
	r.mu.Unlock()

	item := resetTopTrackItem(base)
	item.Kind = kind
	item.MatchKey = matchKey
	r.persistRecommendationItems([]TopTrackItem{item})
	r.emitTopTrackUpdate([]TopTrackItem{item})

	sources := r.topTrackSources(ctx)
	if len(sources) == 0 {
		item.Matching = false
		item.NoSource = true
		r.persistRecommendationItems([]TopTrackItem{item})
		r.emitTopTrackUpdate([]TopTrackItem{item})
		return nil
	}

	state := &topTrackState{item: item, tried: make(map[string]bool)}
	r.markMatchingActive(kind, true)
	r.enqueueTopTrackRun(username, queuedTopTrackRun{
		kind:    kind,
		states:  []*topTrackState{state},
		sources: sources,
	})
	return nil
}

// recommendationBaseRow returns the row's Last.fm identity from memory, or from
// the store after a restart.
func (r *Recommender) recommendationBaseRow(
	ctx context.Context, kind, matchKey string,
) (TopTrackItem, bool) {
	r.mu.Lock()
	base, ok := r.topRows[recommendationRowKey(kind, matchKey)]
	r.mu.Unlock()
	if ok {
		return base, true
	}
	store := r.recommendationStoreOrNil()
	if store == nil {
		return TopTrackItem{}, false
	}
	list, err := store.GetRecommendationList(ctx, kind)
	if err != nil || list == nil {
		return TopTrackItem{}, false
	}
	for _, row := range list.Items {
		if row.MatchKey == matchKey {
			return recommendationItemFromDomain(row), true
		}
	}
	return TopTrackItem{}, false
}

// buildStoredLists builds the requested lists into the store. Top tracks are one
// request; the similar list streams per seed so the page can render it early.
func (r *Recommender) buildStoredLists(
	ctx context.Context, wantTop, wantSimilar bool,
) error {
	tracks, err := r.client.GetTopTracks(ctx, topTrackPeriod, topTrackLimit)
	if err != nil {
		return err
	}
	if wantTop {
		if err := r.storeTopList(ctx, tracks); err != nil {
			return err
		}
	}
	if wantSimilar {
		if _, err := r.streamSimilarList(ctx, tracks); err != nil {
			return err
		}
	}
	return nil
}

// storeTopList replaces the top list and starts its matching.
func (r *Recommender) storeTopList(ctx context.Context, tracks []LastFmTrack) error {
	items := r.buildTopTrackItems(tracks)
	if err := r.saveRecommendationList(ctx, domain.LastFmRecommendationTopTracks, items); err != nil {
		return err
	}
	r.startKindMatching(ctx, domain.LastFmRecommendationTopTracks, items)
	return nil
}

// streamSimilarList rebuilds the similar list seed by seed.
//
// Each seed's new rows are written to the store and emitted as they arrive, so
// the detail page can render the list while the remaining requests run. The
// order is seed order then Last.fm order; the same song is kept once.
func (r *Recommender) streamSimilarList(
	ctx context.Context, tracks []LastFmTrack,
) ([]TopTrackItem, error) {
	// Replace the old content only after the seed request succeeded: a total
	// failure before this point keeps the previous list.
	if err := r.saveRecommendationList(ctx, domain.LastFmRecommendationSimilar, []TopTrackItem{}); err != nil {
		return nil, err
	}

	seeds := usableSeeds(tracks, similarSeedLimit)
	seen := make(map[string]struct{}, len(seeds))
	for _, seed := range seeds {
		seen[similarRecommendationKey(seed)] = struct{}{}
	}

	items := make([]TopTrackItem, 0, similarRecommendationTake)
	for index, seed := range seeds {
		entries := r.similarSeedTracks(ctx, seed, index)
		added := make([]TopTrackItem, 0)
		for _, track := range entries {
			if len(items) >= similarRecommendationTake {
				break
			}
			if !usableTrack(track) {
				continue
			}
			key := similarRecommendationKey(track)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			item := TopTrackItem{
				Title:    track.Title,
				Artist:   track.Artist,
				Album:    track.Album,
				Duration: track.Duration,
				Artists:  []string{track.Artist},
				CoverURL: track.ImageURL,
				Kind:     domain.LastFmRecommendationSimilar,
				Matching: true,
				MatchKey: candidateKey(track.Artist, track.Title),
			}
			items = append(items, item)
			added = append(added, item)
		}
		if len(added) > 0 {
			// Persist before emitting, so a page that reconnects reads the same
			// rows the event announced.
			if err := r.saveRecommendationList(ctx, domain.LastFmRecommendationSimilar, items); err != nil {
				return items, err
			}
			r.emitRecommendationItemsAdded(domain.LastFmRecommendationSimilar, added)
		}
		if len(items) >= similarRecommendationTake {
			break
		}
	}
	r.startKindMatching(ctx, domain.LastFmRecommendationSimilar, items)
	return items, nil
}

// similarSeedTracks returns one seed's rows, using the neighbour fallback when
// the direct answer is empty.
func (r *Recommender) similarSeedTracks(
	ctx context.Context, seed LastFmTrack, index int,
) []LastFmTrack {
	tracks, err := r.client.GetSimilarTracks(ctx, seed.Artist, seed.Title, similarTrackLimit)
	if err != nil {
		r.logger.Debug("last.fm similar tracks failed",
			"artist", seed.Artist, "title", seed.Title, "error", err)
		return nil
	}
	if len(tracks) > 0 || index >= fallbackSeedLimit {
		return tracks
	}
	hits, failed := r.fallbackForSeed(ctx, seed)
	if failed {
		r.logger.Debug("last.fm fallback failed", "artist", seed.Artist, "title", seed.Title)
	}
	fallback := make([]LastFmTrack, 0, len(hits))
	for _, hit := range hits {
		fallback = append(fallback, hit.track)
	}
	return fallback
}

// startKindMatching applies cached matches and queues one list's rows.
func (r *Recommender) startKindMatching(ctx context.Context, kind string, items []TopTrackItem) {
	if len(items) == 0 {
		return
	}
	collected := recommendationCollection{
		status:  StatusOK,
		sources: r.topTrackSources(ctx),
	}
	if kind == domain.LastFmRecommendationTopTracks {
		collected.topItems = items
		collected.wantTop = true
	} else {
		collected.similarItems = items
		collected.wantSimilar = true
	}
	r.startRecommendationMatching(ctx, collected, false)
}

// saveRecommendationList replaces one list's rows.
func (r *Recommender) saveRecommendationList(ctx context.Context, kind string, items []TopTrackItem) error {
	store := r.recommendationStoreOrNil()
	if store == nil {
		return nil
	}
	return store.SaveRecommendationList(ctx, &domain.RecommendationList{
		Kind:      kind,
		Title:     recommendationTitle(kind),
		Items:     toDomainRecommendationItems(items),
		UpdatedAt: r.now().UTC(),
	})
}

// emitRecommendationItemsAdded announces the rows a seed just produced.
func (r *Recommender) emitRecommendationItemsAdded(kind string, items []TopTrackItem) {
	if r.emitter == nil || len(items) == 0 {
		return
	}
	r.emitter(recommendationItemsAddedEvent, RecommendationItemsAdded{
		Kind:  kind,
		Items: items,
	})
}

// persistRecommendationItems writes match progress back to the stored rows.
func (r *Recommender) persistRecommendationItems(items []TopTrackItem) {
	store := r.recommendationStoreOrNil()
	if store == nil || len(items) == 0 {
		return
	}
	rows := make([]domain.RecommendationItem, 0, len(items))
	for _, item := range items {
		if item.Kind == "" || item.MatchKey == "" {
			continue
		}
		rows = append(rows, toDomainRecommendationItem(item))
	}
	if len(rows) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recommendationStoreTimeout)
	defer cancel()
	if err := store.UpdateRecommendationItems(ctx, rows); err != nil {
		r.logger.Warn("last.fm recommendations: persist match", "error", err)
	}
}

// resumeRecommendationMatching restarts the worker for rows a previous session
// left matching. onlyKind limits it to the detail page's list.
func (r *Recommender) resumeRecommendationMatching(ctx context.Context, onlyKind string) {
	store := r.recommendationStoreOrNil()
	if store == nil {
		return
	}
	pending, err := store.PendingRecommendationItems(ctx)
	if err != nil {
		r.logger.Warn("last.fm recommendations: load pending rows", "error", err)
		return
	}
	if len(pending) == 0 {
		return
	}

	groups := make(map[string][]TopTrackItem)
	for _, row := range pending {
		if onlyKind != "" && row.Kind != onlyKind {
			continue
		}
		groups[row.Kind] = append(groups[row.Kind], recommendationItemFromDomain(row))
	}

	_, username, _ := r.client.GetStatus()
	sources := r.topTrackSources(ctx)
	for _, kind := range recommendationKindOrder() {
		items := groups[kind]
		if len(items) == 0 || r.isMatchingActive(kind) {
			continue
		}
		states := make([]*topTrackState, 0, len(items))
		for _, item := range items {
			state := &topTrackState{item: item, tried: make(map[string]bool)}
			for source := range r.failedTopSources(kind, item.MatchKey) {
				state.tried[source] = true
			}
			states = append(states, state)
		}
		r.markMatchingActive(kind, true)
		r.enqueueTopTrackRun(username, queuedTopTrackRun{
			kind:      kind,
			states:    states,
			sources:   sources,
			finalDone: true,
		})
	}
}

func (r *Recommender) markMatchingActive(kind string, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if kind == "" {
		for _, known := range recommendationKindOrder() {
			if active {
				r.matchingActive[known] = true
			} else {
				delete(r.matchingActive, known)
			}
		}
		return
	}
	if active {
		r.matchingActive[kind] = true
		return
	}
	delete(r.matchingActive, kind)
}

func (r *Recommender) isMatchingActive(kind string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.matchingActive[kind]
}

func hasRecommendationKind(summaries []domain.RecommendationListSummary, kind string) bool {
	for _, summary := range summaries {
		if summary.Kind == kind {
			return true
		}
	}
	return false
}

// orderSummaries puts "for you" above the top tracks and drops unknown kinds.
func orderSummaries(
	summaries []domain.RecommendationListSummary,
) []RecommendationListSummary {
	byKind := make(map[string]domain.RecommendationListSummary, len(summaries))
	for _, summary := range summaries {
		byKind[summary.Kind] = summary
	}
	result := make([]RecommendationListSummary, 0, len(summaries))
	for _, kind := range recommendationKindOrder() {
		summary, ok := byKind[kind]
		if !ok {
			continue
		}
		result = append(result, RecommendationListSummary{
			Kind:          summary.Kind,
			Title:         summary.Title,
			TrackCount:    summary.TrackCount,
			MatchingCount: summary.MatchingCount,
			TotalDuration: summary.TotalDuration,
			CoverURLs:     append([]string{}, summary.CoverURLs...),
			UpdatedAt:     summary.UpdatedAt,
		})
	}
	return result
}

// summariesFromItems is the store-less path: the freshly built rows are the
// cards.
func summariesFromItems(topItems, similarItems []TopTrackItem) []RecommendationListSummary {
	result := make([]RecommendationListSummary, 0, 2)
	for _, kind := range recommendationKindOrder() {
		items := topItems
		if kind == domain.LastFmRecommendationSimilar {
			items = similarItems
		}
		summaries := recommendationSummary(kind, items)
		result = append(result, summaries)
	}
	return result
}

func recommendationSummary(kind string, items []TopTrackItem) RecommendationListSummary {
	summary := RecommendationListSummary{
		Kind:       kind,
		Title:      recommendationTitle(kind),
		TrackCount: len(items),
		CoverURLs:  []string{},
	}
	for _, item := range items {
		summary.TotalDuration += item.Duration
		if item.Matching {
			summary.MatchingCount++
		}
		if item.CoverURL == "" || len(summary.CoverURLs) >= 4 {
			continue
		}
		duplicate := false
		for _, existing := range summary.CoverURLs {
			if existing == item.CoverURL {
				duplicate = true
				break
			}
		}
		if !duplicate {
			summary.CoverURLs = append(summary.CoverURLs, item.CoverURL)
		}
	}
	return summary
}

func recommendationListFromDomain(list domain.RecommendationList) RecommendationList {
	items := make([]TopTrackItem, 0, len(list.Items))
	for _, item := range list.Items {
		items = append(items, recommendationItemFromDomain(item))
	}
	return RecommendationList{
		Kind:      list.Kind,
		Title:     list.Title,
		Items:     items,
		UpdatedAt: list.UpdatedAt,
	}
}

func recommendationItemFromDomain(item domain.RecommendationItem) TopTrackItem {
	artists := append([]string{}, item.Artists...)
	if len(artists) == 0 && strings.TrimSpace(item.Artist) != "" {
		artists = []string{item.Artist}
	}
	return TopTrackItem{
		Kind:          item.Kind,
		MatchKey:      item.MatchKey,
		Title:         item.Title,
		Artist:        item.Artist,
		Album:         item.Album,
		Duration:      item.Duration,
		Artists:       artists,
		CoverURL:      item.CoverURL,
		CoverURLLarge: item.CoverURLLarge,
		Matching:      item.Matching,
		Matched:       item.Matched,
		NoSource:      item.NoSource,
		Provider:      item.Provider,
		ExternalID:    item.ExternalID,
		State:         item.State,
		LocalTrackID:  item.LocalTrackID,
		BlockedReason: item.BlockedReason,
		Sources:       append([]string{}, item.Sources...),
		Downloadable:  item.Downloadable,
		Suspicious:    item.Suspicious,
	}
}

func toDomainRecommendationItems(items []TopTrackItem) []domain.RecommendationItem {
	rows := make([]domain.RecommendationItem, 0, len(items))
	for _, item := range items {
		rows = append(rows, toDomainRecommendationItem(item))
	}
	return rows
}

func toDomainRecommendationItem(item TopTrackItem) domain.RecommendationItem {
	return domain.RecommendationItem{
		Kind:          item.Kind,
		MatchKey:      item.MatchKey,
		Title:         item.Title,
		Artist:        item.Artist,
		Artists:       append([]string{}, item.Artists...),
		Album:         item.Album,
		Duration:      item.Duration,
		CoverURL:      item.CoverURL,
		CoverURLLarge: item.CoverURLLarge,
		Downloadable:  item.Downloadable,
		BlockedReason: item.BlockedReason,
		Sources:       append([]string{}, item.Sources...),
		Matching:      item.Matching,
		Matched:       item.Matched,
		NoSource:      item.NoSource,
		Provider:      item.Provider,
		ExternalID:    item.ExternalID,
		State:         item.State,
		LocalTrackID:  item.LocalTrackID,
		Suspicious:    item.Suspicious,
	}
}
