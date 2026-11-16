package lastfm

import (
	"context"
	"fmt"
	"sort"
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
	WeekFrom      int64     `json:"week_from"`
	WeekTo        int64     `json:"week_to"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// RecommendationList is one list's rows in Last.fm order.
//
// WeekFrom/WeekTo and PrevFrom/NextFrom are only set for the weekly echo list:
// they drive the detail page's week switch. HasMore tells the discover page
// whether its candidate pool still has another page.
type RecommendationList struct {
	Kind      string         `json:"kind"`
	Title     string         `json:"title"`
	Items     []TopTrackItem `json:"items"`
	UpdatedAt time.Time      `json:"updated_at"`
	WeekFrom  int64          `json:"week_from"`
	WeekTo    int64          `json:"week_to"`
	PrevFrom  int64          `json:"prev_from"`
	NextFrom  int64          `json:"next_from"`
	HasMore   bool           `json:"has_more"`
}

// RecommendationListCollection is the cards page payload. Status is the same
// machine string the old page used: ok or not_connected.
type RecommendationListCollection struct {
	Status string                      `json:"status"`
	Lists  []RecommendationListSummary `json:"lists"`
}

// recommendationKindOrder is the card order: discovery, the weekly echo, the
// "for you" list, then the all-time top tracks.
func recommendationKindOrder() []string {
	return []string{
		domain.LastFmRecommendationDiscover,
		domain.LastFmRecommendationWeeklyEcho,
		domain.LastFmRecommendationSimilar,
		domain.LastFmRecommendationTopTracks,
	}
}

// recommendationTitle returns the i18n key the page words. The key is stored
// with the list, so a list added later only needs an entry here.
func recommendationTitle(kind string) string {
	switch recommendationBaseKind(kind) {
	case domain.LastFmRecommendationTopTracks:
		return "lastfm.top"
	case domain.LastFmRecommendationSimilar:
		return "lastfm.similar_tracks"
	case domain.LastFmRecommendationWeeklyEcho:
		return "lastfm.weekly_echo"
	case domain.LastFmRecommendationDiscover:
		return "lastfm.discover"
	default:
		return kind
	}
}

func knownRecommendationKind(kind string) bool {
	switch recommendationBaseKind(kind) {
	case domain.LastFmRecommendationTopTracks,
		domain.LastFmRecommendationSimilar,
		domain.LastFmRecommendationWeeklyEcho,
		domain.LastFmRecommendationDiscover:
		return true
	default:
		return false
	}
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
	return r.listRecommendationLists(ctx, false)
}

// ListRecommendationListsWithRefresh also checks the discover TTL and whether a
// new completed week exists. The page's auto-refresh toggle decides whether to
// pass true; the explicit Refresh methods always rebuild.
func (r *Recommender) ListRecommendationListsWithRefresh(
	ctx context.Context, autoRefresh bool,
) (RecommendationListCollection, error) {
	return r.listRecommendationLists(ctx, autoRefresh)
}

func (r *Recommender) listRecommendationLists(
	ctx context.Context, autoRefresh bool,
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
	discoverSummary, hasDiscover := summaryByBase(summaries, domain.LastFmRecommendationDiscover)
	wantDiscover := !hasDiscover ||
		(autoRefresh && r.now().Sub(discoverSummary.UpdatedAt) >= discoverTTL)
	weeklySummary, hasWeekly := latestWeeklyEchoSummary(summaries)
	wantWeekly := !hasWeekly || (autoRefresh && r.weeklyEchoAdvanced(ctx, weeklySummary))

	if missingTop || missingSimilar || wantDiscover {
		tracks, fetchErr := r.client.GetTopTracks(ctx, topTrackPeriod, topTrackLimit)
		if fetchErr != nil {
			if missingTop || missingSimilar {
				return RecommendationListCollection{Status: StatusOK, Lists: orderSummaries(summaries)}, fetchErr
			}
			// The existing cards stay usable; only the rebuild is skipped.
			r.logger.Warn("last.fm recommendations: discovery rebuild failed", "error", fetchErr)
		} else {
			if missingTop {
				if buildErr := r.storeTopList(ctx, tracks); buildErr != nil {
					return RecommendationListCollection{Status: StatusOK, Lists: orderSummaries(summaries)}, buildErr
				}
			}
			if missingSimilar {
				if _, buildErr := r.streamSimilarList(ctx, tracks); buildErr != nil {
					return RecommendationListCollection{Status: StatusOK, Lists: orderSummaries(summaries)}, buildErr
				}
			}
			if wantDiscover {
				if buildErr := r.buildDiscoverList(ctx, tracks); buildErr != nil {
					r.logger.Warn("last.fm recommendations: discovery build failed", "error", buildErr)
				}
			}
		}
	}

	if wantWeekly {
		// A failed or empty chart list must not hide the other cards.
		if _, weeklyErr := r.getWeeklyEchoList(ctx, 0); weeklyErr != nil {
			r.logger.Debug("last.fm weekly echo: build failed", "error", weeklyErr)
		}
	}

	if missingTop || missingSimilar || wantDiscover || wantWeekly {
		summaries, err = store.ListRecommendationLists(ctx)
		if err != nil {
			return empty, err
		}
	}

	r.resumeRecommendationMatching(ctx, "")
	return RecommendationListCollection{Status: StatusOK, Lists: orderSummaries(summaries)}, nil
}

// summaryByBase finds the stored summary of one base kind.
func summaryByBase(
	summaries []domain.RecommendationListSummary, kind string,
) (domain.RecommendationListSummary, bool) {
	for _, summary := range summaries {
		if recommendationBaseKind(summary.Kind) == kind {
			return summary, true
		}
	}
	return domain.RecommendationListSummary{}, false
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

	switch recommendationBaseKind(kind) {
	case domain.LastFmRecommendationWeeklyEcho:
		from, _ := weeklyEchoFrom(kind)
		return r.getWeeklyEchoList(ctx, from)
	case domain.LastFmRecommendationDiscover:
		return r.discoverList(ctx)
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

	switch recommendationBaseKind(kind) {
	case domain.LastFmRecommendationDiscover:
		return r.refreshDiscover(ctx)
	case domain.LastFmRecommendationWeeklyEcho:
		from, _ := weeklyEchoFrom(kind)
		return r.RefreshWeeklyEcho(ctx, from)
	}

	tracks, err := r.client.GetTopTracks(ctx, topTrackPeriod, topTrackLimit)
	if err != nil {
		return err
	}
	if recommendationBaseKind(kind) == domain.LastFmRecommendationTopTracks {
		return r.storeTopList(ctx, tracks)
	}
	_, err = r.streamSimilarList(ctx, tracks)
	return err
}

// RefreshAllRecommendationLists rebuilds every list. The top list is written
// first; the similar list then streams in while its rows are matched. The
// discovery and weekly rebuilds degrade to a logged failure instead of hiding
// the lists that did rebuild.
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
	if _, err := r.streamSimilarList(ctx, tracks); err != nil {
		return err
	}
	if err := r.buildDiscoverList(ctx, tracks); err != nil {
		r.logger.Warn("last.fm recommendations: discovery refresh failed", "error", err)
	}
	if err := r.RefreshWeeklyEcho(ctx, 0); err != nil {
		r.logger.Warn("last.fm weekly echo: refresh failed", "error", err)
	}
	return nil
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
	kind = r.resolveRecommendationKind(ctx, kind)
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
	connected, username, _ := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		return
	}
	sources := r.topTrackSources(ctx)
	states := r.seedTopTrackStates(items, sources)
	r.persistMatchingStates(states)
	if anyTopStatePending(states) {
		r.markMatchingActive(kind, true)
		r.startTopTrackMatching(username, kind, states, sources)
	}
}

// resolveRecommendationKind names the stored week when the caller passes the
// base "weekly_echo" kind: the rows live under "weekly_echo:<from>".
func (r *Recommender) resolveRecommendationKind(ctx context.Context, kind string) string {
	if !isWeeklyEchoKind(kind) {
		return kind
	}
	if _, ok := weeklyEchoFrom(kind); ok {
		return kind
	}
	store := r.recommendationStoreOrNil()
	if store == nil {
		return kind
	}
	summaries, err := store.ListRecommendationLists(ctx)
	if err != nil {
		return kind
	}
	latest, ok := latestWeeklyEchoSummary(summaries)
	if !ok {
		return kind
	}
	return latest.Kind
}

// saveRecommendationList replaces one list's rows.
func (r *Recommender) saveRecommendationList(ctx context.Context, kind string, items []TopTrackItem) error {
	store := r.recommendationStoreOrNil()
	if store == nil {
		return nil
	}
	var weekFrom, weekTo int64
	if from, ok := weeklyEchoFrom(kind); ok {
		weekFrom = from
		for _, item := range items {
			if item.WeekTo > weekTo {
				weekTo = item.WeekTo
			}
		}
	}
	return store.SaveRecommendationList(ctx, &domain.RecommendationList{
		Kind:      kind,
		Title:     recommendationTitle(kind),
		Items:     toDomainRecommendationItems(items),
		UpdatedAt: r.now().UTC(),
		WeekFrom:  weekFrom,
		WeekTo:    weekTo,
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

// RefreshAfterMerge moves the recommendation rows matched through the identity
// a merge just moved onto a local survivor. The survivor keeps the remote
// identity, so one lookup names both the affected rows and their new local id.
func (r *Recommender) RefreshAfterMerge(ctx context.Context, toTrackID string) {
	toTrackID = strings.TrimSpace(toTrackID)
	if toTrackID == "" {
		return
	}
	r.mu.Lock()
	lookup := r.tracks
	r.mu.Unlock()
	if lookup == nil {
		return
	}
	survivor, err := lookup.GetByID(ctx, toTrackID)
	if err != nil {
		r.logger.Warn("last.fm recommendations: load merged survivor",
			"track_id", toTrackID, "error", err)
		return
	}
	r.refreshLocalMatch(survivor, true)
}

// RefreshLocalTrack mirrors library:track-updated for an open page. Only rows
// already cached in memory are refreshed, so a bulk import cannot scan the
// stored lists once per file.
func (r *Recommender) RefreshLocalTrack(track *domain.TrackDTO) {
	r.refreshLocalMatch(track, false)
}

// refreshLocalMatch rewrites rows that were matched through the remote identity
// a local row now owns. includeStored also scans the persisted lists, which is
// needed when the page that shows the row is closed.
func (r *Recommender) refreshLocalMatch(track *domain.TrackDTO, includeStored bool) {
	if track == nil || track.IsRemote() {
		return
	}
	localTrackID := strings.TrimSpace(track.ID)
	if localTrackID == "" {
		return
	}
	provider := domain.NormalizeProvider(track.Provider)
	externalID := strings.TrimSpace(track.ExternalID)
	matchKeys := localMatchKeys(track)

	updates := make([]TopTrackItem, 0, 2)
	seen := make(map[string]struct{})
	add := func(item TopTrackItem) {
		if item.Kind == "" || item.MatchKey == "" {
			return
		}
		key := recommendationRowKey(item.Kind, item.MatchKey)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		applyLocalLibraryRow(&item, localTrackID)
		updates = append(updates, item)
	}

	// Rows of the open page: the per-source cache names them by identity, and
	// topRows keeps the complete Last.fm row the frontend renders.
	if provider != "" && externalID != "" {
		for _, ref := range r.topTrackKeysForIdentity(provider, externalID) {
			r.mu.Lock()
			base, ok := r.topRows[recommendationRowKey(ref.kind, ref.matchKey)]
			r.mu.Unlock()
			if ok {
				add(base)
			}
		}
	}

	// Match-key fallback: a survivor whose identity was not carried still
	// resolves the rows whose Last.fm title/artist names the same song.
	if len(matchKeys) > 0 {
		r.mu.Lock()
		for _, item := range r.topRows {
			if _, ok := matchKeys[item.MatchKey]; ok {
				add(item)
			}
		}
		r.mu.Unlock()
	}

	// Persisted rows of every list, including lists that are not open right now.
	// Weekly lists are stored per week, so the summaries name every kind.
	if includeStored {
		if store := r.recommendationStoreOrNil(); store != nil {
			storedSummons, err := store.ListRecommendationLists(context.Background())
			if err != nil {
				r.logger.Warn("last.fm recommendations: list stored rows", "error", err)
			}
			for _, summary := range storedSummons {
				list, err := store.GetRecommendationList(context.Background(), summary.Kind)
				if err != nil {
					r.logger.Warn("last.fm recommendations: refresh stored rows",
						"kind", summary.Kind, "error", err)
					continue
				}
				if list == nil {
					continue
				}
				for _, item := range list.Items {
					if !identityMatches(item.Provider, item.ExternalID, provider, externalID) {
						if _, ok := matchKeys[item.MatchKey]; !ok {
							continue
						}
					}
					add(recommendationItemFromDomain(item))
				}
			}
		}
	}

	if len(updates) == 0 {
		return
	}

	// The per-source cache must not hand the old provider back on the next page.
	r.dropSourceMatches(updates, provider, externalID)
	r.persistRecommendationItems(updates)
	// One event per row: an update carries a single kind, and the row may belong
	// to either list.
	for _, item := range updates {
		r.emitTopTrackUpdate([]TopTrackItem{item})
	}
}

// dropSourceMatches clears cached source outcomes for rows that are now local,
// whether the cache was found by row identity or by the row's match key.
func (r *Recommender) dropSourceMatches(items []TopTrackItem, provider, externalID string) {
	if len(items) == 0 {
		return
	}
	refs := make(map[string]struct{}, len(items))
	for _, item := range items {
		refs[recommendationRowKey(item.Kind, item.MatchKey)] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for cacheKey, entry := range r.sourceMatches {
		index := strings.LastIndexByte(cacheKey, '|')
		if index <= 0 {
			continue
		}
		ref := splitRecommendationRowKey(cacheKey[:index])
		if _, ok := refs[recommendationRowKey(ref.kind, ref.matchKey)]; ok {
			delete(r.sourceMatches, cacheKey)
			continue
		}
		if entry.row != nil && identityMatches(entry.row.Provider, entry.row.ExternalID, provider, externalID) {
			delete(r.sourceMatches, cacheKey)
		}
	}
}

// identityMatches reports whether a row was matched through the given identity.
// Both sides are normalised, so "WY" and " wy " still match.
func identityMatches(provider, externalID, wantProvider, wantExternalID string) bool {
	if wantProvider == "" || wantExternalID == "" {
		return false
	}
	return domain.NormalizeProvider(provider) == wantProvider &&
		strings.TrimSpace(externalID) == wantExternalID
}

// applyLocalLibraryRow rewrites a matched row to the local copy that now owns
// the audio: the source identity is dropped because "already in library" must
// outrank "from <provider>", and nothing remains to download.
func applyLocalLibraryRow(item *TopTrackItem, localTrackID string) {
	item.Provider = ""
	item.ExternalID = ""
	item.Sources = []string{}
	item.State = domain.CatalogRowStateInLibrary
	item.LocalTrackID = localTrackID
	item.Matching = false
	item.Matched = true
	item.NoSource = false
}

// localMatchKeys returns every Last.fm match key the local row could stand for.
// artist+title is what the recommendation rows are keyed by.
func localMatchKeys(track *domain.TrackDTO) map[string]struct{} {
	keys := make(map[string]struct{})
	title := strings.TrimSpace(track.Title)
	if title == "" {
		return keys
	}
	artists := make([]string, 0, len(track.Artists)+1)
	for _, artist := range track.Artists {
		if artist != nil && strings.TrimSpace(artist.Name) != "" {
			artists = append(artists, strings.TrimSpace(artist.Name))
		}
	}
	if len(artists) == 0 && strings.TrimSpace(track.RawArtistNames) != "" {
		artists = append(artists, strings.TrimSpace(track.RawArtistNames))
	}
	for _, artist := range artists {
		keys[candidateKey(artist, title)] = struct{}{}
	}
	return keys
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
	kinds := make([]string, 0, len(groups))
	for kind := range groups {
		if knownRecommendationKind(kind) {
			kinds = append(kinds, kind)
		}
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
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

// orderSummaries puts the cards in page order and drops unknown kinds. Weekly
// lists are persisted per week, so only the newest one becomes the card.
func orderSummaries(
	summaries []domain.RecommendationListSummary,
) []RecommendationListSummary {
	byKind := make(map[string]domain.RecommendationListSummary, len(summaries))
	for _, summary := range summaries {
		base := recommendationBaseKind(summary.Kind)
		existing, ok := byKind[base]
		if !ok || (isWeeklyEchoKind(base) && summary.WeekFrom > existing.WeekFrom) {
			summary.Kind = base
			byKind[base] = summary
		}
	}
	result := make([]RecommendationListSummary, 0, len(summaries))
	for _, kind := range recommendationKindOrder() {
		summary, ok := byKind[kind]
		if !ok {
			continue
		}
		result = append(result, RecommendationListSummary{
			Kind:          summary.Kind,
			Title:         recommendationTitle(kind),
			TrackCount:    summary.TrackCount,
			MatchingCount: summary.MatchingCount,
			TotalDuration: summary.TotalDuration,
			CoverURLs:     append([]string{}, summary.CoverURLs...),
			WeekFrom:      summary.WeekFrom,
			WeekTo:        summary.WeekTo,
			UpdatedAt:     summary.UpdatedAt,
		})
	}
	return result
}

// summariesFromItems is the store-less path: the freshly built rows are the
// cards. Only the two in-memory lists exist there.
func summariesFromItems(topItems, similarItems []TopTrackItem) []RecommendationListSummary {
	result := make([]RecommendationListSummary, 0, 2)
	for _, kind := range []string{
		domain.LastFmRecommendationSimilar,
		domain.LastFmRecommendationTopTracks,
	} {
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
		WeekFrom:  list.WeekFrom,
		WeekTo:    list.WeekTo,
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
		PlayCount:     item.PlayCount,
		WeekFrom:      item.WeekFrom,
		WeekTo:        item.WeekTo,
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
		PlayCount:     item.PlayCount,
		WeekFrom:      item.WeekFrom,
		WeekTo:        item.WeekTo,
	}
}
