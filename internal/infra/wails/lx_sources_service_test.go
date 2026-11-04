package wails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"airmedy/internal/app/lx"
	"airmedy/internal/domain"
)

// lxRepo is an in-memory LxSourceRepository for the binding test.
type lxRepo struct {
	mu      sync.Mutex
	sources map[string]domain.LxSource
}

func newLxRepo() *lxRepo {
	return &lxRepo{sources: make(map[string]domain.LxSource)}
}

func (r *lxRepo) List(context.Context) ([]domain.LxSource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sources := make([]domain.LxSource, 0, len(r.sources))
	for _, source := range r.sources {
		sources = append(sources, source)
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Position != sources[j].Position {
			return sources[i].Position < sources[j].Position
		}
		return sources[i].ID < sources[j].ID
	})
	return sources, nil
}

func (r *lxRepo) Create(_ context.Context, source domain.LxSource) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sources[source.ID] = source
	return nil
}

func (r *lxRepo) Update(_ context.Context, source domain.LxSource) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.sources[source.ID]
	if !ok {
		return fmt.Errorf("%w: %q", domain.ErrLxSourceNotFound, source.ID)
	}
	existing.Name = source.Name
	existing.Enabled = source.Enabled
	r.sources[source.ID] = existing
	return nil
}

func (r *lxRepo) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sources, id)
	return nil
}

func (r *lxRepo) Reorder(_ context.Context, ids []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	position := 0
	for _, id := range ids {
		existing, ok := r.sources[id]
		if !ok {
			continue
		}
		existing.Position = position
		r.sources[id] = existing
		position++
	}
	return nil
}

// lxRegistry is the registry slice the binding's service needs.
type lxRegistry struct {
	mu sync.Mutex

	running     bool
	describe    domain.PluginDescribe
	describeErr error
	validation  domain.PluginSourceValidation
	callErr     error
}

func (r *lxRegistry) Host(context.Context, string) (domain.PluginHost, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return nil, r.running
}

func (r *lxRegistry) Start(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = true
	return nil
}

func (r *lxRegistry) Stop(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = false
	return nil
}

func (r *lxRegistry) Restart(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = true
	return nil
}

func (r *lxRegistry) Describe(context.Context, string) (domain.PluginDescribe, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.describeErr != nil {
		return domain.PluginDescribe{}, r.describeErr
	}
	return r.describe, nil
}

func (r *lxRegistry) Call(_ context.Context, _ string, _ string, _ any, result any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.callErr != nil {
		return r.callErr
	}
	if target, ok := result.(*domain.PluginSourceValidation); ok {
		*target = r.validation
	}
	return nil
}

func discardLxLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newLxFixture(t *testing.T) (*LxSourcesService, *lxRepo, *lxRegistry, string) {
	t.Helper()
	dir := t.TempDir()
	repo := newLxRepo()
	registry := &lxRegistry{running: true}
	service := lx.NewService(repo, discardLxLogger(), lx.Config{SourcesDir: dir})
	service.AttachRegistry(registry)
	return NewLxSourcesService(service), repo, registry, dir
}

// seedLxSource stores one row without going through import (which needs a real
// plugin).
func seedLxSource(repo *lxRepo, id, name string, enabled bool, position int, dir string) {
	repo.sources[id] = domain.LxSource{
		ID:       id,
		Name:     name,
		FilePath: filepath.Join(dir, id+".js"),
		Enabled:  enabled,
		Position: position,
	}
}

// TestListLxSourcesJoinsRowsWithDescribe is the binding contract the page renders:
// the row says what the user imported, describe says what each source can serve.
func TestListLxSourcesJoinsRowsWithDescribe(t *testing.T) {
	service, repo, registry, dir := newLxFixture(t)
	seedLxSource(repo, "a1", "ikun", true, 0, dir)
	seedLxSource(repo, "b2", "off", false, 1, dir)
	registry.describe = domain.PluginDescribe{
		Providers: []string{"wy", "kg"},
		Sources: []domain.PluginSourceDescription{
			{ID: "a1", Name: "ikun", Enabled: true, Providers: []string{"wy", "kg"}},
			{ID: "b2", Name: "off", Enabled: false, Providers: []string{"tx"}},
		},
	}

	state, err := service.ListLxSources(context.Background())
	if err != nil {
		t.Fatalf("ListLxSources: %v", err)
	}
	if !state.PluginRunning {
		t.Fatal("PluginRunning must be true when describe answered")
	}
	if state.PluginError != "" {
		t.Fatalf("PluginError = %q, want empty for a healthy plugin", state.PluginError)
	}
	if state.Directory != dir {
		t.Fatalf("Directory = %q, want %q", state.Directory, dir)
	}
	if len(state.Sources) != 2 {
		t.Fatalf("Sources = %+v, want two rows", state.Sources)
	}
	if got := state.Sources[0].Providers; len(got) != 2 || got[0] != "wy" {
		t.Fatalf("a1 providers = %v, want the describe answer", got)
	}
	if state.Sources[0].FileName != "a1.js" {
		t.Fatalf("FileName = %q, want the stored base name", state.Sources[0].FileName)
	}
	if state.Sources[1].Enabled {
		t.Fatal("b2 is disabled and must render as disabled")
	}
	if got := state.Sources[1].Providers; len(got) != 1 || got[0] != "tx" {
		t.Fatalf("b2 providers = %v, want the describe answer", got)
	}
}

// TestListLxSourcesExplainsAStoppedPlugin: when describe cannot answer, the page
// must say why instead of showing every source as unknown.
func TestListLxSourcesExplainsAStoppedPlugin(t *testing.T) {
	service, repo, registry, dir := newLxFixture(t)
	seedLxSource(repo, "a1", "ikun", true, 0, dir)
	registry.running = false

	state, err := service.ListLxSources(context.Background())
	if err != nil {
		t.Fatalf("ListLxSources must not fail when the plugin is off: %v", err)
	}
	if state.PluginRunning {
		t.Fatal("PluginRunning must be false when the plugin is off")
	}
	if state.PluginError == "" {
		t.Fatal("the state must explain why providers are unknown")
	}
	// The rows are still listed: the user manages the list even with the plugin
	// turned off.
	if len(state.Sources) != 1 || state.Sources[0].ID != "a1" {
		t.Fatalf("Sources = %+v, want the stored row", state.Sources)
	}
	if state.Sources[0].Providers == nil {
		t.Fatal("Providers must encode as [] rather than null")
	}
}

// TestListLxSourcesIsEmptyNotNil keeps the JSON contract stable for the empty
// state.
func TestListLxSourcesIsEmptyNotNil(t *testing.T) {
	service, _, registry, _ := newLxFixture(t)
	registry.describe = domain.PluginDescribe{}
	state, err := service.ListLxSources(context.Background())
	if err != nil {
		t.Fatalf("ListLxSources: %v", err)
	}
	if state.Sources == nil {
		t.Fatal("Sources must be an empty slice, not nil")
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) == "" {
		t.Fatal("state must encode")
	}
}

// TestImportLxSourceReturnsTheRow exercises the import path end to end with the
// picker replaced by a direct path, and asserts the DTO the page appends.
func TestImportLxSourceReturnsTheRow(t *testing.T) {
	service, _, registry, _ := newLxFixture(t)
	registry.running = true
	registry.validation = domain.PluginSourceValidation{Name: "ikun", Providers: []string{"wy"}}

	candidate := filepath.Join(t.TempDir(), "ikun.js")
	if err := os.WriteFile(candidate, []byte("globalThis.__source = 1;"), 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	// The binding opens a dialog, which a unit test cannot drive; import through
	// the service the binding wraps, then check the DTO mapping on the row.
	row, err := service.svc.Import(context.Background(), candidate)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	dto := LxSourceDTO{
		ID:        row.ID,
		Name:      row.Name,
		FileName:  baseName(row.FilePath),
		Enabled:   row.Enabled,
		Position:  row.Position,
		Providers: []string{},
	}
	if dto.Name != "ikun" || !dto.Enabled || dto.FileName != row.ID+".js" {
		t.Fatalf("dto = %+v, want the imported row", dto)
	}
}

// TestRenameLxSourcePassesThrough checks the binding is a translation layer only:
// the label reaches the service and the sentinel survives the error path.
func TestRenameLxSourcePassesThrough(t *testing.T) {
	service, repo, _, dir := newLxFixture(t)
	seedLxSource(repo, "a1", "ikun", true, 0, dir)

	if err := service.RenameLxSource(context.Background(), "a1", "我的源"); err != nil {
		t.Fatalf("RenameLxSource: %v", err)
	}
	state, err := service.ListLxSources(context.Background())
	if err != nil {
		t.Fatalf("ListLxSources: %v", err)
	}
	if state.Sources[0].Name != "我的源" {
		t.Fatalf("Name = %q, want the new label", state.Sources[0].Name)
	}

	err = service.RenameLxSource(context.Background(), "ghost", "x")
	if !errors.Is(err, domain.ErrLxSourceNotFound) {
		t.Fatalf("RenameLxSource(ghost) = %v, want the sentinel to survive", err)
	}
}

// TestTranslateLxErrors turns each sentinel into text a user can act on, and
// keeps the sentinel reachable through errors.Is.
func TestTranslateLxErrors(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{domain.ErrLxSourceInvalid, "cannot be used"},
		{domain.ErrLxSourceNotFound, "no longer in the list"},
		{domain.ErrLxSourceLimit, "too many sources"},
		{domain.ErrLxPluginUnavailable, "not available"},
	}
	for _, testCase := range cases {
		translated := translateLxError(fmt.Errorf("%w: detail", testCase.err))
		if translated == nil {
			t.Fatalf("translate(%v) = nil", testCase.err)
		}
		if !strings.Contains(translated.Error(), testCase.want) {
			t.Fatalf("translate(%v) = %q, want %q", testCase.err, translated.Error(), testCase.want)
		}
		if !errors.Is(translated, testCase.err) {
			t.Fatalf("translate(%v) lost the sentinel", testCase.err)
		}
	}
}
