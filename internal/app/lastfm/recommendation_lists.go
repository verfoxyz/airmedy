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
		collected, buildErr := r.collectRecommendations(ctx, missingTop, missingSimilar, false)
		if buildErr != nil {
			return RecommendationListCollection{Status: collected.status, Lists: orderSummaries(summaries)}, buildErr
		}
		if collected.status != StatusNotConnected {
			if storeErr := r.storeCollectedLists(ctx, collected); storeErr != nil {
				return RecommendationListCollection{Status: collected.status, Lists: orderSummaries(summaries)}, storeErr
			}
			r.startRecommendationMatching(ctx, collected, missingTop && missingSimilar)
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
// with the same collect-then-store flow, so matching never races the insert.
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
		collected, buildErr := r.collectRecommendations(
			ctx, kind == domain.LastFmRecommendationTopTracks, kind == domain.LastFmRecommendationSimilar, false)
		if buildErr != nil {
			return RecommendationList{Kind: kind, Title: recommendationTitle(kind)}, buildErr
		}
		if collected.status != StatusNotConnected {
			if storeErr := r.storeCollectedLists(ctx, collected); storeErr != nil {
				return RecommendationList{Kind: kind, Title: recommendationTitle(kind)}, storeErr
			}
			r.startRecommendationMatching(ctx, collected, false)
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
	} else {
		r.resumeRecommendationMatching(ctx, kind)
	}
	return recommendationListFromDomain(*list), nil
}

// RefreshRecommendationList rebuilds exactly one list. A failed build keeps the
// stored rows and their match state untouched.
func (r *Recommender) RefreshRecommendationList(ctx context.Context, kind string) error {
	if !knownRecommendationKind(kind) {
		return fmt.Errorf("lastfm recommendations: unknown list %q", kind)
	}
	connected, username, _ := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		return nil
	}

	collected, err := r.collectRecommendations(
		ctx, kind == domain.LastFmRecommendationTopTracks, kind == domain.LastFmRecommendationSimilar, true)
	if err != nil {
		return err
	}
	if collected.status == StatusNotConnected {
		return nil
	}
	// Persist before matching starts, so the first update finds its row.
	if err := r.storeCollectedLists(ctx, collected); err != nil {
		return err
	}
	r.startRecommendationMatching(ctx, collected, false)
	return nil
}

// RefreshAllRecommendationLists rebuilds both lists. The rows are replaced per
// list, so a failure leaves the failed list's previous content in place.
func (r *Recommender) RefreshAllRecommendationLists(ctx context.Context) error {
	connected, username, _ := r.client.GetStatus()
	if !connected || strings.TrimSpace(username) == "" {
		return nil
	}

	collected, err := r.collectRecommendations(ctx, true, true, true)
	if err != nil {
		return err
	}
	if collected.status == StatusNotConnected {
		return nil
	}
	if err := r.storeCollectedLists(ctx, collected); err != nil {
		return err
	}
	r.startRecommendationMatching(ctx, collected, true)
	return nil
}

// storeCollectedLists persists the lists the collection contains.
func (r *Recommender) storeCollectedLists(ctx context.Context, collected recommendationCollection) error {
	store := r.recommendationStoreOrNil()
	if store == nil {
		return nil
	}
	updatedAt := r.now().UTC()
	if collected.wantTop {
		list := &domain.RecommendationList{
			Kind:      domain.LastFmRecommendationTopTracks,
			Title:     recommendationTitle(domain.LastFmRecommendationTopTracks),
			Items:     toDomainRecommendationItems(collected.topItems),
			UpdatedAt: updatedAt,
		}
		if err := store.SaveRecommendationList(ctx, list); err != nil {
			return err
		}
	}
	if collected.wantSimilar {
		list := &domain.RecommendationList{
			Kind:      domain.LastFmRecommendationSimilar,
			Title:     recommendationTitle(domain.LastFmRecommendationSimilar),
			Items:     toDomainRecommendationItems(collected.similarItems),
			UpdatedAt: updatedAt,
		}
		if err := store.SaveRecommendationList(ctx, list); err != nil {
			return err
		}
	}
	return nil
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
	}
}
