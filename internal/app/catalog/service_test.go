package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"airmedy/internal/domain"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func row(id, title string, artists ...string) domain.PluginSearchResultRow {
	return domain.PluginSearchResultRow{ExternalID: id, Title: title, Artists: artists}
}

// fakeSources is a static source table.
type fakeSources map[string]domain.CatalogSearchSource

func (f fakeSources) SearchSources(context.Context) (map[string]domain.CatalogSearchSource, error) {
	return f, nil
}

// fakeSearcher answers per source, and can be told to fail, block or record calls.
type fakeSearcher struct {
	results map[string]domain.PluginSearchResult
	errs    map[string]error
	// block waits for a channel per source before answering, which is how a
	// test proves one slow source does not hold the others.
	block map[string]chan struct{}
	// delay is applied before answering, for the timeout case.
	delay time.Duration

	mu    sync.Mutex
	calls []domain.PluginSearchRequest
}

func (f *fakeSearcher) SearchSource(ctx context.Context, source string, request domain.PluginSearchRequest) (domain.PluginSearchResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, request)
	err := f.errs[source]
	block := f.block[source]
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return domain.PluginSearchResult{}, ctx.Err()
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return domain.PluginSearchResult{}, ctx.Err()
		}
	}
	if err != nil {
		return domain.PluginSearchResult{}, err
	}
	return f.results[source], nil
}

func (f *fakeSearcher) called() []domain.PluginSearchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.PluginSearchRequest(nil), f.calls...)
}

// fakeResolver reports a fixed set of downloadable providers.
type fakeResolver map[string]bool

func (f fakeResolver) CanResolve(provider string) bool { return f[provider] }

func wySource() domain.CatalogSearchSource {
	return domain.CatalogSearchSource{ID: "wy", Name: "网易云"}
}
func kgSource() domain.CatalogSearchSource {
	return domain.CatalogSearchSource{ID: "kg", Name: "酷狗"}
}

// --- ranking ----------------------------------------------------------------

// TestFuseRanks_UsesPositionNotScore pins the fusion contract: a source's numeric
// "relevance score" never enters the maths, only its row order.
func TestFuseRanks_UsesPositionNotScore(t *testing.T) {
	results := map[string]domain.PluginSearchResult{
		"wy": {Rows: []domain.PluginSearchResultRow{row("1", "first"), row("2", "second")}},
		"kg": {Rows: []domain.PluginSearchResultRow{row("k1", "k-first")}},
	}
	fused := fuseRanks("", []string{"wy", "kg"}, results)

	if len(fused) != 3 {
		t.Fatalf("fused %d rows, want 3", len(fused))
	}
	// Rank 0 from either source scores 1/(0+3) = 0.333; the tie breaks on the
	// declared source order, so wy's first row comes before kg's.
	if fused[0].Provider != "wy" || fused[1].Provider != "kg" {
		t.Fatalf("order = %s, %s; want wy, kg for tied rank-0 rows",
			fused[0].Provider, fused[1].Provider)
	}
}

// TestFuseRanks_TitleMatchBeatsLowerRank: a row whose title matches the query
// must outrank a better-positioned row that does not match it.
func TestFuseRanks_TitleMatchBeatsLowerRank(t *testing.T) {
	results := map[string]domain.PluginSearchResult{
		"wy": {
			Rows: []domain.PluginSearchResultRow{
				row("a", "unrelated"),
				row("b", "晴天"),
			},
		},
	}
	fused := fuseRanks("晴天", []string{"wy"}, results)
	if len(fused) != 2 {
		t.Fatalf("fused %d rows, want 2", len(fused))
	}
	if fused[0].Title != "晴天" {
		t.Fatalf("first row = %q, want the title match ranked first", fused[0].Title)
	}
}

// TestFuseRanks_ArtistMatchCounts: the bonus covers artists too, because users
// search by artist as often as by title.
func TestFuseRanks_ArtistMatchCounts(t *testing.T) {
	results := map[string]domain.PluginSearchResult{
		"wy": {
			Rows: []domain.PluginSearchResultRow{
				row("a", "some song", "other artist"),
				row("b", "another song", "周杰伦"),
			},
		},
	}
	fused := fuseRanks("周杰伦", []string{"wy"}, results)
	if fused[0].ExternalID != "b" {
		t.Fatalf("first row = %q, want the artist match", fused[0].ExternalID)
	}
}

// TestFuseRanks_DeduplicatesPerSourceIdentity: the same (source, id) twice is one
// row, while the same id from *another* source stays separate (D5: no cross
// source merge).
func TestFuseRanks_DeduplicatesPerSourceIdentity(t *testing.T) {
	results := map[string]domain.PluginSearchResult{
		"wy": {Rows: []domain.PluginSearchResultRow{row("x", "t"), row("x", "t again"), row("y", "u")}},
		"kg": {Rows: []domain.PluginSearchResultRow{row("x", "same id, different source")}},
	}
	fused := fuseRanks("", []string{"wy", "kg"}, results)

	if len(fused) != 3 {
		t.Fatalf("fused %d rows, want 3 (wy/x, wy/y, kg/x)", len(fused))
	}
	var wyCount int
	for _, entry := range fused {
		if entry.Provider == "wy" && entry.ExternalID == "x" {
			wyCount++
		}
	}
	if wyCount != 1 {
		t.Fatalf("wy/x appears %d times, want 1", wyCount)
	}
}

// TestFuseRanks_DropsRowsWithoutAnID: an id-less row can be neither downloaded
// nor identified, so rendering it would offer an action that cannot work.
func TestFuseRanks_DropsRowsWithoutAnID(t *testing.T) {
	results := map[string]domain.PluginSearchResult{
		"wy": {Rows: []domain.PluginSearchResultRow{row("", "no id"), row("  ", "blank id"), row("ok", "fine")}},
	}
	fused := fuseRanks("", []string{"wy"}, results)
	if len(fused) != 1 || fused[0].ExternalID != "ok" {
		t.Fatalf("fused = %+v, want only the row with an id", fused)
	}
}

// TestFuseRanks_IsDeterministic: two runs over the same answers produce the same
// order, so the page does not shuffle between identical searches.
func TestFuseRanks_IsDeterministic(t *testing.T) {
	results := map[string]domain.PluginSearchResult{
		"wy": {Rows: []domain.PluginSearchResultRow{row("1", "a"), row("2", "b")}},
		"kg": {Rows: []domain.PluginSearchResultRow{row("3", "c"), row("4", "d")}},
	}
	first := fuseRanks("", []string{"wy", "kg"}, results)
	for attempt := 0; attempt < 20; attempt++ {
		again := fuseRanks("", []string{"wy", "kg"}, results)
		if len(again) != len(first) {
			t.Fatalf("attempt %d produced %d rows, want %d", attempt, len(again), len(first))
		}
		for index := range first {
			// Artists is a slice, so compare the identity that ordering is
			// actually about rather than the whole struct.
			if first[index].Provider != again[index].Provider ||
				first[index].ExternalID != again[index].ExternalID {
				t.Fatalf("attempt %d differs at %d: %+v vs %+v",
					attempt, index, first[index], again[index])
			}
		}
	}
}

// --- paging -----------------------------------------------------------------

func TestPageOf(t *testing.T) {
	rows := make([]domain.CatalogSearchResult, 0, 120)
	for index := 0; index < 120; index++ {
		rows = append(rows, domain.CatalogSearchResult{ExternalID: fmt.Sprintf("id-%03d", index)})
	}

	page, hasMore := pageOf(rows, 0, 50)
	if len(page) != 50 || !hasMore {
		t.Fatalf("page 1 = %d rows, hasMore=%v; want 50, true", len(page), hasMore)
	}
	page, hasMore = pageOf(rows, 100, 50)
	if len(page) != 20 || hasMore {
		t.Fatalf("page 3 = %d rows, hasMore=%v; want 20, false", len(page), hasMore)
	}
	page, hasMore = pageOf(rows, 120, 50)
	if len(page) != 0 || hasMore {
		t.Fatalf("past the end = %d rows, hasMore=%v; want 0, false", len(page), hasMore)
	}
}

// TestPageOf_HasMoreIsNotAGuess: a source that returns exactly Limit rows and has
// nothing behind them must not make the UI offer an empty "load more".
func TestPageOf_HasMoreIsNotAGuess(t *testing.T) {
	rows := make([]domain.CatalogSearchResult, 0, 50)
	for index := 0; index < 50; index++ {
		rows = append(rows, domain.CatalogSearchResult{ExternalID: fmt.Sprintf("%d", index)})
	}
	page, hasMore := pageOf(rows, 0, 50)
	if len(page) != 50 {
		t.Fatalf("page = %d rows, want 50", len(page))
	}
	if hasMore {
		t.Fatal("hasMore = true for a list that ended exactly at the page size")
	}
}

// --- service ----------------------------------------------------------------

// fakeStates answers the row-state port and records what it was asked.
type fakeStates struct {
	states       map[domain.CatalogIdentity]string
	localMatches map[domain.CatalogIdentity]string
	err          error

	mu    sync.Mutex
	asks  [][]domain.CatalogIdentity
	calls int
}

func (f *fakeStates) CatalogRowStates(_ context.Context, identities []domain.CatalogIdentity) (map[domain.CatalogIdentity]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.asks = append(f.asks, append([]domain.CatalogIdentity(nil), identities...))
	return f.states, f.err
}

func (f *fakeStates) CatalogLocalMatches(_ context.Context, _ []domain.CatalogLocalQuery) (map[domain.CatalogIdentity]string, error) {
	return f.localMatches, f.err
}

func TestApplyStates_MatchesAnImportedLocalRow(t *testing.T) {
	identity := domain.CatalogIdentity{Provider: "wy", ExternalID: "509781655"}
	states := &fakeStates{localMatches: map[domain.CatalogIdentity]string{identity: "local-1"}}
	svc := NewService(Deps{States: states, Logger: quietLogger()})

	rows := []domain.CatalogSearchResult{{
		Provider: "wy", ExternalID: "509781655", Title: "晴天", Artists: []string{"周杰伦"}, Duration: 239,
	}}
	svc.applyStates(context.Background(), rows)

	if rows[0].LocalTrackID != "local-1" || rows[0].State != domain.CatalogRowStateInLibrary {
		t.Fatalf("row = %+v, want the local candidate reported as in_library", rows[0])
	}
}

func TestApplyStates_RegisteredIdentityWinsOverALocalLookalike(t *testing.T) {
	identity := domain.CatalogIdentity{Provider: "wy", ExternalID: "509781655"}
	states := &fakeStates{
		states:       map[domain.CatalogIdentity]string{identity: domain.CatalogRowStateDownloading},
		localMatches: map[domain.CatalogIdentity]string{identity: "local-1"},
	}
	svc := NewService(Deps{States: states, Logger: quietLogger()})

	rows := []domain.CatalogSearchResult{{
		Provider: "wy", ExternalID: "509781655", Title: "晴天", Duration: 239,
	}}
	svc.applyStates(context.Background(), rows)

	if rows[0].State != domain.CatalogRowStateDownloading || rows[0].LocalTrackID != "" {
		t.Fatalf("row = %+v, want the registered state to win", rows[0])
	}
}

func (f *fakeStates) asked() [][]domain.CatalogIdentity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]domain.CatalogIdentity(nil), f.asks...)
}

func newTestService(sources fakeSources, searcher *fakeSearcher, resolver fakeResolver, config Config) *Service {
	return NewService(Deps{
		Sources:  sources,
		Searcher: searcher,
		Resolver: resolver,
		Logger:   quietLogger(),
		Config:   config,
	})
}

// newTestServiceWithStates builds a service that can also answer row states.
func newTestServiceWithStates(
	sources fakeSources, searcher *fakeSearcher, resolver fakeResolver, states domain.CatalogRowStateResolver, config Config,
) *Service {
	return NewService(Deps{
		Sources:  sources,
		Searcher: searcher,
		Resolver: resolver,
		States:   states,
		Logger:   quietLogger(),
		Config:   config,
	})
}

// TestSearch_MergesSourcesAndPages: the headline path — two sources, one page,
// stable order.
func TestSearch_MergesSourcesAndPages(t *testing.T) {
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {Rows: []domain.PluginSearchResultRow{row("w1", "晴天"), row("w2", "夜曲")}},
		"kg": {Rows: []domain.PluginSearchResultRow{row("k1", "晴天")}},
	}}
	service := newTestService(
		fakeSources{"wy": wySource(), "kg": kgSource()},
		searcher,
		fakeResolver{"wy": true},
		Config{},
	)

	page, err := service.Search(context.Background(), domain.CatalogSearchRequest{Query: "晴天"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(page.Rows))
	}
	if len(page.Sources) != 2 {
		t.Fatalf("sources = %+v, want both", page.Sources)
	}
	// Both source buttons and every row's provider must be the normalised id.
	for _, source := range page.Sources {
		if domain.NormalizeProvider(source.ID) != source.ID {
			t.Fatalf("source id %q is not normalised", source.ID)
		}
	}
	// One call per selected source, never one call for "everything": the merged
	// ranking is a host concern, and each plugin owns one source.
	if calls := searcher.called(); len(calls) != 2 {
		t.Fatalf("searcher called %d times, want one per source", len(calls))
	}
}

// TestSearch_NoSourcesIsItsOwnError: "nothing installed" must be distinguishable
// from "this query matched nothing", because the UI explains them differently.
func TestSearch_NoSourcesIsItsOwnError(t *testing.T) {
	service := newTestService(fakeSources{}, &fakeSearcher{}, fakeResolver{}, Config{})

	_, err := service.Search(context.Background(), domain.CatalogSearchRequest{Query: "x"})
	if !errors.Is(err, domain.ErrCatalogNoSources) {
		t.Fatalf("Search with no sources = %v, want ErrCatalogNoSources", err)
	}
}

// TestSearch_PartialResultsSurvive: one failing source must not blank the page.
func TestSearch_PartialResultsSurvive(t *testing.T) {
	searcher := &fakeSearcher{
		results: map[string]domain.PluginSearchResult{
			"wy": {Rows: []domain.PluginSearchResultRow{row("w1", "ok")}},
		},
		errs: map[string]error{"kg": errors.New("upstream 502")},
	}
	service := newTestService(
		fakeSources{"wy": wySource(), "kg": kgSource()},
		searcher,
		fakeResolver{"wy": true},
		Config{},
	)

	page, err := service.Search(context.Background(), domain.CatalogSearchRequest{Query: "ok"})
	if err == nil {
		t.Fatal("Search = nil error, want the failed source reported")
	}
	if !errors.Is(err, domain.ErrCatalogSearchFailed) {
		t.Fatalf("error = %v, want ErrCatalogSearchFailed", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("rows = %d, want the one that answered", len(page.Rows))
	}
}

// TestSearch_SlowSourceDoesNotBlockTheOthers: the fan-out is concurrent and each
// source has its own budget.
func TestSearch_SlowSourceDoesNotBlockTheOthers(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	searcher := &fakeSearcher{
		results: map[string]domain.PluginSearchResult{
			"wy": {Rows: []domain.PluginSearchResultRow{row("w1", "fast")}},
			"kg": {Rows: []domain.PluginSearchResultRow{row("k1", "slow")}},
		},
		block: map[string]chan struct{}{"kg": release},
	}
	service := newTestService(
		fakeSources{"wy": wySource(), "kg": kgSource()},
		searcher,
		fakeResolver{"wy": true},
		Config{SourceTimeout: 200 * time.Millisecond},
	)

	started := time.Now()
	page, _ := service.Search(context.Background(), domain.CatalogSearchRequest{Query: "x"})
	elapsed := time.Since(started)

	if elapsed > 5*time.Second {
		t.Fatalf("Search took %s, want it bounded by the source timeout", elapsed)
	}
	if len(page.Rows) != 1 || page.Rows[0].ExternalID != "w1" {
		t.Fatalf("rows = %+v, want only the fast source's row", page.Rows)
	}
}

// TestSearch_AvailabilityMarksUndownloadableRows: a source that can search but
// nothing can resolve is disabled with a reason, not silently broken.
func TestSearch_AvailabilityMarksUndownloadableRows(t *testing.T) {
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {Rows: []domain.PluginSearchResultRow{row("w1", "resolvable")}},
		"kg": {Rows: []domain.PluginSearchResultRow{row("k1", "not resolvable")}},
	}}
	// Only wy has a resolver: kg can search but nothing can fetch it.
	service := newTestService(
		fakeSources{"wy": wySource(), "kg": kgSource()},
		searcher,
		fakeResolver{"wy": true},
		Config{},
	)

	page, err := service.Search(context.Background(), domain.CatalogSearchRequest{Query: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, entry := range page.Rows {
		switch entry.Provider {
		case "wy":
			if !entry.Downloadable {
				t.Fatalf("wy row is not downloadable: %+v", entry)
			}
		case "kg":
			if entry.Downloadable {
				t.Fatalf("kg row is downloadable but nothing resolves it: %+v", entry)
			}
			if entry.DownloadBlockedReason == "" {
				t.Fatalf("kg row has no reason: %+v", entry)
			}
		}
	}
}

// TestSearch_FilterSelectsOneSource: the UI's per-source button must actually
// narrow the search, not just reorder it.
func TestSearch_FilterSelectsOneSource(t *testing.T) {
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {Rows: []domain.PluginSearchResultRow{row("w1", "wy only")}},
	}}
	service := newTestService(
		fakeSources{"wy": wySource(), "kg": kgSource()},
		searcher,
		fakeResolver{"wy": true},
		Config{},
	)

	page, err := service.Search(context.Background(), domain.CatalogSearchRequest{
		Query: "x", Sources: []string{"wy"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Rows) != 1 || page.Rows[0].Provider != "wy" {
		t.Fatalf("rows = %+v, want only the filtered source", page.Rows)
	}
	if calls := searcher.called(); len(calls) != 1 || calls[0].Source != "wy" {
		t.Fatalf("calls = %+v, want exactly the wy call", calls)
	}
	// The button row still shows every available source, so the user can switch
	// back without the list collapsing to what they filtered.
	if len(page.Sources) != 2 {
		t.Fatalf("sources = %+v, want the full table even when filtered", page.Sources)
	}
}

// TestSearch_UnknownFilterNamesWhatWasAsked: filtering to a source nobody offers
// is a routing miss with the requested ids in the message.
func TestSearch_UnknownFilterNamesWhatWasAsked(t *testing.T) {
	service := newTestService(fakeSources{"wy": wySource()}, &fakeSearcher{}, fakeResolver{}, Config{})

	_, err := service.Search(context.Background(), domain.CatalogSearchRequest{
		Query: "x", Sources: []string{"tx"},
	})
	if !errors.Is(err, domain.ErrCatalogSourceUnavailable) {
		t.Fatalf("Search = %v, want ErrCatalogSourceUnavailable", err)
	}
	if !strings.Contains(err.Error(), "tx") {
		t.Fatalf("error = %v, want it to name the requested source", err)
	}
}

// TestSearch_BlankQueryIsRejected covers the validation the binding relies on.
func TestSearch_BlankQueryIsRejected(t *testing.T) {
	service := newTestService(fakeSources{"wy": wySource()}, &fakeSearcher{}, fakeResolver{}, Config{})

	_, err := service.Search(context.Background(), domain.CatalogSearchRequest{Query: "   "})
	if !errors.Is(err, domain.ErrCatalogSearchInvalid) {
		t.Fatalf("Search with a blank query = %v, want ErrCatalogSearchInvalid", err)
	}
}

// TestSearch_AsksForOffsetPlusLimit: paging needs the sources to return enough
// rows to cover page 2, or the second page has no rows to rank.
func TestSearch_AsksForOffsetPlusLimit(t *testing.T) {
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {Rows: []domain.PluginSearchResultRow{row("w1", "x")}},
	}}
	service := newTestService(fakeSources{"wy": wySource()}, searcher, fakeResolver{"wy": true}, Config{})

	if _, err := service.Search(context.Background(), domain.CatalogSearchRequest{
		Query: "x", Offset: 50, Limit: 50,
	}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	calls := searcher.called()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Limit > domain.MaxCatalogSearchPageSize {
		t.Fatalf("Limit = %d, want it capped at %d", calls[0].Limit, domain.MaxCatalogSearchPageSize)
	}
}

// TestSearch_NilSearcherFailsLoudly: a deployment without the capability must
// produce the documented error, not a panic or an empty page.
func TestSearch_NilSearcherFailsLoudly(t *testing.T) {
	service := NewService(Deps{Sources: fakeSources{"wy": wySource()}, Logger: quietLogger()})

	_, err := service.Search(context.Background(), domain.CatalogSearchRequest{Query: "x"})
	if !errors.Is(err, domain.ErrCatalogSearchFailed) {
		t.Fatalf("Search without a searcher = %v, want ErrCatalogSearchFailed", err)
	}
}

// --- row states --------------------------------------------------------------

// TestSearch_StampsLibraryStates: each rendered row carries what the library
// already knows about its identity, so the UI never offers a second download.
func TestSearch_StampsLibraryStates(t *testing.T) {
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {Rows: []domain.PluginSearchResultRow{row("w1", "晴天"), row("w2", "夜曲"), row("w3", "彩虹")}},
	}}
	states := &fakeStates{states: map[domain.CatalogIdentity]string{
		{Provider: "wy", ExternalID: "w1"}: domain.CatalogRowStateInLibrary,
		{Provider: "wy", ExternalID: "w2"}: domain.CatalogRowStateDownloading,
	}}
	service := newTestServiceWithStates(
		fakeSources{"wy": wySource()}, searcher, fakeResolver{"wy": true}, states, Config{},
	)

	page, err := service.Search(context.Background(), domain.CatalogSearchRequest{Query: "周杰伦"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	byID := make(map[string]string, len(page.Rows))
	for _, r := range page.Rows {
		byID[r.ExternalID] = r.State
	}
	if byID["w1"] != domain.CatalogRowStateInLibrary {
		t.Fatalf("w1 state = %q, want in_library", byID["w1"])
	}
	if byID["w2"] != domain.CatalogRowStateDownloading {
		t.Fatalf("w2 state = %q, want downloading", byID["w2"])
	}
	if byID["w3"] != "" {
		t.Fatalf("w3 state = %q, want empty for an unregistered track", byID["w3"])
	}

	asks := states.asked()
	if len(asks) != 1 {
		t.Fatalf("state lookups = %d, want one batch per page", len(asks))
	}
	if len(asks[0]) != 3 {
		t.Fatalf("asked for %d identities, want the three rendered rows", len(asks[0]))
	}
}

// TestSearch_StateLookupFailureKeepsThePage: a broken badge must not blank a
// working search.
func TestSearch_StateLookupFailureKeepsThePage(t *testing.T) {
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {Rows: []domain.PluginSearchResultRow{row("w1", "晴天")}},
	}}
	states := &fakeStates{err: errors.New("database is locked")}
	service := newTestServiceWithStates(
		fakeSources{"wy": wySource()}, searcher, fakeResolver{"wy": true}, states, Config{},
	)

	page, err := service.Search(context.Background(), domain.CatalogSearchRequest{Query: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("rows = %d, want the row even without a state", len(page.Rows))
	}
	if page.Rows[0].State != "" {
		t.Fatalf("state = %q, want empty after a failed lookup", page.Rows[0].State)
	}
}

// TestSearch_StatesAreOnlyAskedForThePage: rows past the page are not resolved,
// because the caller never renders them.
func TestSearch_StatesAreOnlyAskedForThePage(t *testing.T) {
	rows := make([]domain.PluginSearchResultRow, 0, 60)
	for i := 0; i < 60; i++ {
		rows = append(rows, row(fmt.Sprintf("w%02d", i), fmt.Sprintf("track %d", i)))
	}
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {Rows: rows, HasMore: true},
	}}
	states := &fakeStates{}
	service := newTestServiceWithStates(
		fakeSources{"wy": wySource()}, searcher, fakeResolver{"wy": true}, states, Config{},
	)

	if _, err := service.Search(context.Background(), domain.CatalogSearchRequest{Query: "x"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	asks := states.asked()
	if len(asks) != 1 {
		t.Fatalf("state lookups = %d, want 1", len(asks))
	}
	if len(asks[0]) != domain.MaxCatalogSearchPageSize {
		t.Fatalf("asked for %d identities, want the page size %d", len(asks[0]), domain.MaxCatalogSearchPageSize)
	}
}
