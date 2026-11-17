package catalog

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"airmedy/internal/domain"
)

// fakeSourceOrderRepo is an in-memory preference store.
type fakeSourceOrderRepo struct {
	order    []string
	disabled []string
	saved    []string
	loads    int
	saveErr  error
	loadErr  error
}

func (f *fakeSourceOrderRepo) Load(context.Context) (domain.CatalogSourcePreferences, error) {
	f.loads++
	if f.loadErr != nil {
		return domain.CatalogSourcePreferences{}, f.loadErr
	}
	return domain.CatalogSourcePreferences{
		Order:    append([]string{}, f.order...),
		Disabled: append([]string{}, f.disabled...),
	}, nil
}

func (f *fakeSourceOrderRepo) SaveOrder(_ context.Context, order []string) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = append([]string{}, order...)
	f.order = append([]string{}, order...)
	return nil
}

func (f *fakeSourceOrderRepo) SetDisabled(_ context.Context, provider string, disabled bool) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	next := make([]string, 0, len(f.disabled)+1)
	for _, entry := range f.disabled {
		if entry != provider {
			next = append(next, entry)
		}
	}
	if disabled {
		next = append(next, provider)
	}
	f.disabled = next
	return nil
}

func sourceMap(ids ...string) map[string]domain.CatalogSearchSource {
	result := make(map[string]domain.CatalogSearchSource, len(ids))
	for _, id := range ids {
		result[id] = domain.CatalogSearchSource{ID: id, Name: id}
	}
	return result
}

func TestSelectSourcesUsesConfiguredOrder(t *testing.T) {
	available := sourceMap("kg", "kw", "mg", "tx", "wy")
	order := []string{"wy", "kw", "tx", "mg", "kg"}

	if got := selectSources(nil, available, order); !reflect.DeepEqual(got, order) {
		t.Fatalf("all sources = %v, want %v", got, order)
	}
	if got := selectSources([]string{"kg", "wy"}, available, order); !reflect.DeepEqual(got, []string{"wy", "kg"}) {
		t.Fatalf("filtered = %v, want the configured relative order", got)
	}
}

func TestSelectSourcesFallsBackToAlphabeticalWithoutOrder(t *testing.T) {
	available := sourceMap("wy", "kg", "tx")
	got := selectSources(nil, available, nil)
	want := []string{"kg", "tx", "wy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("all sources = %v, want %v", got, want)
	}
}

func TestSearchUsesConfiguredOrderForTheTieBreak(t *testing.T) {
	rows := map[string]domain.PluginSearchResult{
		"kg": {Rows: []domain.PluginSearchResultRow{row("k1", "same song", "artist")}},
		"wy": {Rows: []domain.PluginSearchResultRow{row("w1", "same song", "artist")}},
	}
	for _, test := range []struct {
		name  string
		order []string
		want  string
	}{
		{name: "kg last", order: []string{"wy", "kg"}, want: "wy"},
		{name: "kg first", order: []string{"kg", "wy"}, want: "kg"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &fakeSourceOrderRepo{order: test.order}
			service := NewService(Deps{
				Sources:      fakeSources{"kg": kgSource(), "wy": wySource()},
				Searcher:     &fakeSearcher{results: rows},
				SourceOrders: repo,
				Logger:       quietLogger(),
			})

			page, err := service.Search(context.Background(), domain.CatalogSearchRequest{
				Query: "same song", Limit: 10,
			})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(page.Rows) != 1 {
				t.Fatalf("rows = %+v, want one merged row", page.Rows)
			}
			if page.Rows[0].Provider != test.want {
				t.Fatalf("provider = %q, want %q", page.Rows[0].Provider, test.want)
			}
			if got := page.Sources[0].ID; got != test.order[0] {
				t.Fatalf("first chip = %q, want %q", got, test.order[0])
			}
			if repo.loads != 1 {
				t.Fatalf("loads = %d, want the preferences cached after the first read", repo.loads)
			}
		})
	}
}

func TestSearchSkipsDisabledSources(t *testing.T) {
	repo := &fakeSourceOrderRepo{order: []string{"kg", "wy"}, disabled: []string{"kg"}}
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"kg": {Rows: []domain.PluginSearchResultRow{row("k1", "kg only", "artist")}},
		"wy": {Rows: []domain.PluginSearchResultRow{row("w1", "wy only", "artist")}},
	}}
	service := NewService(Deps{
		Sources:      fakeSources{"kg": kgSource(), "wy": wySource()},
		Searcher:     searcher,
		SourceOrders: repo,
		Logger:       quietLogger(),
	})

	page, err := service.Search(context.Background(), domain.CatalogSearchRequest{
		Query: "only", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, call := range searcher.called() {
		if call.Source == "kg" {
			t.Fatal("the disabled source was searched")
		}
	}
	if len(page.Sources) != 1 || page.Sources[0].ID != "wy" {
		t.Fatalf("sources = %+v, want only wy", page.Sources)
	}
	if len(page.Rows) != 1 || page.Rows[0].Provider != "wy" {
		t.Fatalf("rows = %+v, want only the wy row", page.Rows)
	}
}

func TestMatchTrackSkipsDisabledSources(t *testing.T) {
	repo := &fakeSourceOrderRepo{disabled: []string{"kg"}}
	searcher := &fakeSearcher{results: map[string]domain.PluginSearchResult{
		"wy": {Rows: []domain.PluginSearchResultRow{{
			ExternalID: "w1", Title: "Song", Artists: []string{"Artist"}, Duration: 200,
		}}},
	}}
	service := NewService(Deps{
		Sources:      fakeSources{"kg": kgSource(), "wy": wySource()},
		Searcher:     searcher,
		SourceOrders: repo,
		Logger:       quietLogger(),
	})

	matches, err := service.MatchTrack(context.Background(), "Song", "Artist", 200, nil)
	if err != nil {
		t.Fatalf("MatchTrack: %v", err)
	}
	if len(matches) != 1 || matches[0].Provider != "wy" {
		t.Fatalf("matches = %+v, want the wy row", matches)
	}
	for _, call := range searcher.called() {
		if call.Source == "kg" {
			t.Fatal("matching searched the disabled source")
		}
	}
}

func TestAllSourcesDisabledReturnsErrCatalogNoSources(t *testing.T) {
	repo := &fakeSourceOrderRepo{disabled: []string{"kg", "wy"}}
	service := NewService(Deps{
		Sources:      fakeSources{"kg": kgSource(), "wy": wySource()},
		Searcher:     &fakeSearcher{},
		SourceOrders: repo,
		Logger:       quietLogger(),
	})

	_, err := service.Search(context.Background(), domain.CatalogSearchRequest{
		Query: "anything", Limit: 10,
	})
	if !errors.Is(err, domain.ErrCatalogNoSources) {
		t.Fatalf("error = %v, want ErrCatalogNoSources", err)
	}
	if !errors.Is(err, domain.ErrCatalogAllSourcesDisabled) {
		t.Fatalf("error = %v, want the actionable all-disabled sentinel", err)
	}

	sources, sourcesErr := service.Sources(context.Background())
	if sourcesErr != nil {
		t.Fatalf("Sources: %v", sourcesErr)
	}
	if len(sources) != 0 {
		t.Fatalf("sources = %+v, want none", sources)
	}
}

func TestSourceToggleKeepsOrderAndOrderKeepsDisabled(t *testing.T) {
	repo := &fakeSourceOrderRepo{}
	service := NewService(Deps{
		Sources:      fakeSources{"kg": kgSource(), "wy": wySource()},
		Searcher:     &fakeSearcher{},
		SourceOrders: repo,
		Logger:       quietLogger(),
	})
	ctx := context.Background()

	if _, err := service.SaveSourceOrder(ctx, []string{"wy", "kg"}); err != nil {
		t.Fatalf("SaveSourceOrder: %v", err)
	}
	disabled, err := service.SetSourceDisabled(ctx, "kg", true)
	if err != nil {
		t.Fatalf("SetSourceDisabled: %v", err)
	}
	if !reflect.DeepEqual(disabled, []string{"kg"}) {
		t.Fatalf("disabled = %v, want [kg]", disabled)
	}

	preferences, err := service.SourcePreferences(ctx)
	if err != nil {
		t.Fatalf("SourcePreferences: %v", err)
	}
	if !reflect.DeepEqual(preferences.Order, []string{"wy", "kg"}) {
		t.Fatalf("order = %v, want the disabled source kept in place", preferences.Order)
	}
	if !reflect.DeepEqual(preferences.Disabled, []string{"kg"}) {
		t.Fatalf("disabled = %v, want [kg]", preferences.Disabled)
	}

	// Saving a new order must not re-enable the source.
	if _, err := service.SaveSourceOrder(ctx, []string{"kg", "wy"}); err != nil {
		t.Fatalf("SaveSourceOrder after disable: %v", err)
	}
	preferences, err = service.SourcePreferences(ctx)
	if err != nil {
		t.Fatalf("SourcePreferences: %v", err)
	}
	if !reflect.DeepEqual(preferences.Disabled, []string{"kg"}) {
		t.Fatalf("disabled = %v, want the toggle preserved", preferences.Disabled)
	}

	// Re-enabling puts it back where the order says.
	disabled, err = service.SetSourceDisabled(ctx, "kg", false)
	if err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if len(disabled) != 0 {
		t.Fatalf("disabled = %v, want empty", disabled)
	}
	enabled, err := service.Sources(ctx)
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(enabled) != 2 || enabled[0].ID != "kg" {
		t.Fatalf("sources = %+v, want kg first again", enabled)
	}
}

func TestSetSourceDisabledRejectsUnknownSource(t *testing.T) {
	service := NewService(Deps{
		Sources:      fakeSources{"wy": wySource()},
		Searcher:     &fakeSearcher{},
		SourceOrders: &fakeSourceOrderRepo{},
		Logger:       quietLogger(),
	})
	if _, err := service.SetSourceDisabled(context.Background(), "kg", true); err == nil {
		t.Fatal("SetSourceDisabled accepted an unknown source")
	}
}

func TestSourceOrderReadFailureKeepsAlphabetical(t *testing.T) {
	repo := &fakeSourceOrderRepo{loadErr: errors.New("disk gone")}
	service := NewService(Deps{
		Sources:      fakeSources{"kg": kgSource(), "wy": wySource()},
		Searcher:     &fakeSearcher{},
		SourceOrders: repo,
		Logger:       quietLogger(),
	})

	sources, err := service.Sources(context.Background())
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(sources) != 2 || sources[0].ID != "kg" {
		t.Fatalf("sources = %+v, want the alphabetical fallback", sources)
	}
}
