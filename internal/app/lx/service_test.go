package lx

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

	"airmedy/internal/domain"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// memoryRepo is an in-memory LxSourceRepository that behaves like the SQLite one:
// List is ordered by position then id, Create rejects a duplicate id, and Reorder
// renumbers 0..n-1 over the ids it actually knows.
type memoryRepo struct {
	mu      sync.Mutex
	sources map[string]domain.LxSource
	err     error
	inserts int
}

func newMemoryRepo() *memoryRepo {
	return &memoryRepo{sources: make(map[string]domain.LxSource)}
}

func (r *memoryRepo) List(context.Context) ([]domain.LxSource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	sources := make([]domain.LxSource, 0, len(r.sources))
	for _, source := range r.sources {
		sources = append(sources, source)
	}
	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].Position != sources[j].Position {
			return sources[i].Position < sources[j].Position
		}
		return sources[i].ID < sources[j].ID
	})
	return sources, nil
}

func (r *memoryRepo) Create(_ context.Context, source domain.LxSource) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if _, exists := r.sources[source.ID]; exists {
		return fmt.Errorf("duplicate id %q", source.ID)
	}
	r.inserts++
	r.sources[source.ID] = source
	return nil
}

func (r *memoryRepo) Update(_ context.Context, source domain.LxSource) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	existing, ok := r.sources[source.ID]
	if !ok {
		return fmt.Errorf("%w: %q", domain.ErrLxSourceNotFound, source.ID)
	}
	existing.Name = source.Name
	existing.Enabled = source.Enabled
	r.sources[source.ID] = existing
	return nil
}

func (r *memoryRepo) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	delete(r.sources, id)
	return nil
}

func (r *memoryRepo) Reorder(_ context.Context, ids []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
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

func (r *memoryRepo) seed(sources ...domain.LxSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, source := range sources {
		r.sources[source.ID] = source
	}
}

// fakeRegistry records starts, stops and describes so a test can assert that a
// change restarted the plugin exactly once.
type fakeRegistry struct {
	mu sync.Mutex

	running   bool
	started   int
	stopped   int
	restarts  int
	describes int

	describe    domain.PluginDescribe
	describeErr error
	callErr     error
	validation  domain.PluginSourceValidation
	calls       []string
}

func (r *fakeRegistry) Host(context.Context, string) (domain.PluginHost, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return nil, r.running
}

func (r *fakeRegistry) Start(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = true
	r.started++
	return nil
}

func (r *fakeRegistry) Stop(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = false
	r.stopped++
	return nil
}

func (r *fakeRegistry) Restart(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = true
	r.restarts++
	return nil
}

func (r *fakeRegistry) Describe(context.Context, string) (domain.PluginDescribe, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.describes++
	if r.describeErr != nil {
		return domain.PluginDescribe{}, r.describeErr
	}
	return r.describe, nil
}

func (r *fakeRegistry) Call(_ context.Context, _ string, method string, _ any, result any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, method)
	if r.callErr != nil {
		return r.callErr
	}
	if target, ok := result.(*domain.PluginSourceValidation); ok {
		*target = r.validation
	}
	return nil
}

func (r *fakeRegistry) setRunning(running bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = running
}

func (r *fakeRegistry) counts() (starts, stops, restarts, describes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started, r.stopped, r.restarts, r.describes
}

// newTestService wires the service over a temp sources directory.
func newTestService(t *testing.T) (*Service, *memoryRepo, *fakeRegistry, string) {
	t.Helper()
	dir := t.TempDir()
	repo := newMemoryRepo()
	registry := &fakeRegistry{running: true}
	service := NewService(repo, testLogger(), Config{SourcesDir: dir})
	service.AttachRegistry(registry)
	return service, repo, registry, dir
}

// writeSource writes a candidate script outside the sources directory, the way a
// file the user picked in Downloads would look.
func writeSource(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	return path
}

// TestImportCopiesAndRegisters is acceptance case 1: the file is copied into the
// sources directory under a uuid name, the row carries that path, and the new
// source is enabled by default.
func TestImportCopiesAndRegisters(t *testing.T) {
	service, repo, registry, dir := newTestService(t)
	registry.validation = domain.PluginSourceValidation{Name: "ikun", Providers: []string{"wy", "kg"}}
	candidate := writeSource(t, "ikun-音乐源.js", "globalThis.__source = 1;")

	source, err := service.Import(context.Background(), candidate)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !source.Enabled {
		t.Fatal("an imported source must start enabled")
	}
	if source.Name != "ikun" {
		t.Fatalf("Name = %q, want the label the script declared", source.Name)
	}
	if !withinDir(source.FilePath, dir) {
		t.Fatalf("FilePath = %q, want a copy under %q", source.FilePath, dir)
	}
	if filepath.Base(source.FilePath) != source.ID+domain.LxSourceExtension {
		t.Fatalf("stored file = %q, want <uuid>.js", source.FilePath)
	}
	// The original is untouched and the copy has the same bytes: the user may
	// move or delete the file they picked without breaking playback.
	original, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	copied, err := os.ReadFile(source.FilePath)
	if err != nil {
		t.Fatalf("read copy: %v", err)
	}
	if string(original) != string(copied) {
		t.Fatal("the stored copy must be byte-identical to the imported file")
	}

	stored, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(stored) != 1 || stored[0].ID != source.ID {
		t.Fatalf("stored = %+v, want the imported row", stored)
	}
}

// TestImportRejectsSyntaxError is acceptance case 2: the plugin's validation
// answers with an error, the row is not written, and no copy is left behind.
func TestImportRejectsSyntaxError(t *testing.T) {
	service, repo, registry, dir := newTestService(t)
	registry.callErr = &domain.RPCError{Code: -32000, Message: "boom.js: source script threw during load: SyntaxError"}

	_, err := service.Import(context.Background(), writeSource(t, "boom.js", "not javascript {{{"))
	if !errors.Is(err, domain.ErrLxSourceInvalid) {
		t.Fatalf("Import = %v, want ErrLxSourceInvalid", err)
	}
	if !strings.Contains(err.Error(), "SyntaxError") {
		t.Fatalf("error = %q, want the plugin's reason", err)
	}

	stored, listErr := repo.List(context.Background())
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(stored) != 0 {
		t.Fatalf("stored = %+v, want no row for a rejected import", stored)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr == nil && len(entries) != 0 {
		t.Fatalf("the rejected copy was left in %q: %v", dir, entries)
	}
}

// TestImportRejectsNoProviders is acceptance case 3: a script that loads but
// registers no platform is not a custom source.
func TestImportRejectsNoProviders(t *testing.T) {
	service, _, _, _ := newTestService(t)
	// The plugin reports an empty provider list: it loaded, but declared nothing.
	_, err := service.Import(context.Background(), writeSource(t, "plain.js", "globalThis.__x = 1;"))
	if !errors.Is(err, domain.ErrLxSourceInvalid) {
		t.Fatalf("Import = %v, want ErrLxSourceInvalid", err)
	}
	if !strings.Contains(err.Error(), "does not register any platform") {
		t.Fatalf("error = %q, want the readable reason", err)
	}
}

// TestImportRejectsANonJavaScriptFile keeps the cheap rules ahead of the runtime:
// no process is started for a file that is obviously not a source.
func TestImportRejectsANonJavaScriptFile(t *testing.T) {
	service, _, registry, _ := newTestService(t)
	_, err := service.Import(context.Background(), writeSource(t, "source.txt", "hello"))
	if !errors.Is(err, domain.ErrLxSourceInvalid) {
		t.Fatalf("Import = %v, want ErrLxSourceInvalid", err)
	}
	if _, _, _, _ = registry.counts(); len(registry.calls) != 0 {
		t.Fatalf("validate was called %v, want no call for a .txt file", registry.calls)
	}
}

// TestImportRejectsAnOversizedFile is the size limit from task §2.1.
func TestImportRejectsAnOversizedFile(t *testing.T) {
	service, _, _, _ := newTestService(t)
	path := writeSource(t, "big.js", strings.Repeat("x", int(domain.MaxLxSourceBytes)+16))
	_, err := service.Import(context.Background(), path)
	if !errors.Is(err, domain.ErrLxSourceInvalid) {
		t.Fatalf("Import = %v, want ErrLxSourceInvalid", err)
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error = %q, want the size limit named", err)
	}
}

// TestImportSameFileTwiceGivesTwoRow is acceptance case 4: two imports are two
// independent rows with two independent files.
func TestImportSameFileTwiceGivesTwoRow(t *testing.T) {
	service, repo, registry, _ := newTestService(t)
	registry.validation = domain.PluginSourceValidation{Name: "dup", Providers: []string{"wy"}}
	candidate := writeSource(t, "dup.js", "globalThis.__source = 1;")

	first, err := service.Import(context.Background(), candidate)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	second, err := service.Import(context.Background(), candidate)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("two imports must get two ids")
	}
	if first.FilePath == second.FilePath {
		t.Fatal("two imports must get two stored files")
	}
	stored, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("List = %+v, want two rows", stored)
	}
}

// TestImportRespectsTheLimit: an unbounded list is a UI and memory problem.
func TestImportRespectsTheLimit(t *testing.T) {
	dir := t.TempDir()
	repo := newMemoryRepo()
	registry := &fakeRegistry{running: true, validation: domain.PluginSourceValidation{Providers: []string{"wy"}}}
	service := NewService(repo, testLogger(), Config{SourcesDir: dir, MaxSources: 1})
	service.AttachRegistry(registry)

	if _, err := service.Import(context.Background(), writeSource(t, "one.js", "x")); err != nil {
		t.Fatalf("first import: %v", err)
	}
	_, err := service.Import(context.Background(), writeSource(t, "two.js", "x"))
	if !errors.Is(err, domain.ErrLxSourceLimit) {
		t.Fatalf("second import = %v, want ErrLxSourceLimit", err)
	}
}

// TestImportAppendsAtTheEnd: an import must never silently outrank a source the
// user already trusts.
func TestImportAppendsAtTheEnd(t *testing.T) {
	service, repo, registry, _ := newTestService(t)
	registry.validation = domain.PluginSourceValidation{Providers: []string{"wy"}}
	repo.seed(domain.LxSource{ID: "first", Name: "first", FilePath: "/first.js", Enabled: true, Position: 3})

	source, err := service.Import(context.Background(), writeSource(t, "new.js", "x"))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if source.Position != 4 {
		t.Fatalf("Position = %d, want the position after the last source", source.Position)
	}
}

// TestImportStartsThePluginForValidationAndStopsItAgain: importing through a
// disabled plugin works, and the manager does not stay running afterwards.
func TestImportStartsThePluginForValidationAndStopsItAgain(t *testing.T) {
	service, _, registry, _ := newTestService(t)
	registry.setRunning(false)
	registry.validation = domain.PluginSourceValidation{Providers: []string{"wy"}}

	if _, err := service.Import(context.Background(), writeSource(t, "a.js", "x")); err != nil {
		t.Fatalf("Import with a stopped plugin: %v", err)
	}
	starts, stops, _, _ := registry.counts()
	if starts != 1 || stops != 1 {
		t.Fatalf("starts=%d stops=%d, want the plugin started and stopped once", starts, stops)
	}
	if registry.running {
		t.Fatal("a plugin that was idle before the import must be idle again")
	}
}

// TestImportRestartsARunningPlugin: the new source must be routable immediately.
func TestImportRestartsARunningPlugin(t *testing.T) {
	service, _, registry, _ := newTestService(t)
	registry.validation = domain.PluginSourceValidation{Providers: []string{"wy"}}
	if _, err := service.Import(context.Background(), writeSource(t, "a.js", "x")); err != nil {
		t.Fatalf("Import: %v", err)
	}
	_, _, restarts, _ := registry.counts()
	if restarts != 1 {
		t.Fatalf("restarts = %d, want the plugin restarted so the new list takes effect", restarts)
	}
}

// TestSetEnabledPersistsAndRestarts is acceptance case 8's storage and restart
// half: a disabled source is persisted, and the plugin is restarted so the change
// takes effect.
func TestSetEnabledPersistsAndRestarts(t *testing.T) {
	service, repo, registry, _ := newTestService(t)
	repo.seed(domain.LxSource{ID: "a1", Name: "ikun", FilePath: "/a1.js", Enabled: true})
	registry.setRunning(true)

	if err := service.SetEnabled(context.Background(), "a1", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	stored, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if stored[0].Enabled {
		t.Fatal("the disabled flag must be persisted")
	}
	if _, _, restarts, _ := registry.counts(); restarts != 1 {
		t.Fatalf("restarts = %d, want one restart", restarts)
	}

	// Setting the same value again is a no-op: no pointless restart.
	if err := service.SetEnabled(context.Background(), "a1", false); err != nil {
		t.Fatalf("SetEnabled (same value): %v", err)
	}
	if _, _, restarts, _ := registry.counts(); restarts != 1 {
		t.Fatalf("restarts = %d, want no restart when nothing changed", restarts)
	}
}

// TestSetEnabledDoesNotStartAStoppedPlugin: managing the list must not turn the
// plugin on behind the user's back.
func TestSetEnabledDoesNotStartAStoppedPlugin(t *testing.T) {
	service, repo, registry, _ := newTestService(t)
	repo.seed(domain.LxSource{ID: "a1", Name: "ikun", FilePath: "/a1.js", Enabled: true})
	registry.setRunning(false)

	if err := service.SetEnabled(context.Background(), "a1", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if starts, _, _, _ := registry.counts(); starts != 0 {
		t.Fatalf("starts = %d, want none for a plugin that was off", starts)
	}
}

// TestSetEnabledUnknownID keeps a stale UI row from writing a flag.
func TestSetEnabledUnknownID(t *testing.T) {
	service, _, _, _ := newTestService(t)
	err := service.SetEnabled(context.Background(), "ghost", true)
	if !errors.Is(err, domain.ErrLxSourceNotFound) {
		t.Fatalf("SetEnabled(ghost) = %v, want ErrLxSourceNotFound", err)
	}
}

// TestImportUsesTheDeclaredProviders: the label comes from the script when it has
// one, and the file name otherwise, so the list never shows an empty row.
func TestImportFallsBackToTheFileName(t *testing.T) {
	service, _, registry, _ := newTestService(t)
	// The plugin reports a platform but no name of its own, so the label has to
	// fall back to the file name.
	registry.validation = domain.PluginSourceValidation{Providers: []string{"wy"}}
	source, err := service.Import(context.Background(), writeSource(t, "my-source.js", "x"))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if source.Name != "my-source" {
		t.Fatalf("Name = %q, want the file name without the extension", source.Name)
	}
}

// TestRenameUpdatesTheLabel: the label is user-editable and does not need a
// restart, because the plugin is told file contents, not labels.
func TestRenameUpdatesTheLabel(t *testing.T) {
	service, repo, registry, _ := newTestService(t)
	repo.seed(domain.LxSource{ID: "a1", Name: "ikun", FilePath: "/a1.js", Enabled: true})

	if err := service.Rename(context.Background(), "a1", "  我的源  "); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	sources, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if sources[0].Name != "我的源" {
		t.Fatalf("Name = %q, want the trimmed label", sources[0].Name)
	}
	// A label change must not bounce the plugin process.
	if _, _, restarts, _ := registry.counts(); restarts != 0 {
		t.Fatalf("restarts = %d, want none for a pure label change", restarts)
	}
}

// TestRenameRejectsABlankName keeps an empty row from becoming unclickable.
func TestRenameRejectsABlankName(t *testing.T) {
	service, repo, _, _ := newTestService(t)
	repo.seed(domain.LxSource{ID: "a1", Name: "ikun", FilePath: "/a1.js", Enabled: true})
	if err := service.Rename(context.Background(), "a1", "   "); !errors.Is(err, domain.ErrLxSourceInvalid) {
		t.Fatalf("Rename(blank) = %v, want ErrLxSourceInvalid", err)
	}
}

// TestRenameUnknownID explains a stale rename.
func TestRenameUnknownID(t *testing.T) {
	service, _, _, _ := newTestService(t)
	err := service.Rename(context.Background(), "ghost", "x")
	if !errors.Is(err, domain.ErrLxSourceNotFound) {
		t.Fatalf("Rename(ghost) = %v, want ErrLxSourceNotFound", err)
	}
}

// TestRemoveDeletesRowAndFile is the delete path: the copy goes away with the row
// so the sources directory does not accumulate orphans.
func TestRemoveDeletesRowAndFile(t *testing.T) {
	service, _, registry, _ := newTestService(t)
	registry.validation = domain.PluginSourceValidation{Providers: []string{"wy"}}
	source, err := service.Import(context.Background(), writeSource(t, "a.js", "x"))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if _, err := os.Stat(source.FilePath); err != nil {
		t.Fatalf("stored file missing before delete: %v", err)
	}

	if err := service.Remove(context.Background(), source.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(source.FilePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stored file still exists after remove: %v", err)
	}
	// One restart for the import, one for the delete.
	if _, _, restarts, _ := registry.counts(); restarts != 2 {
		t.Fatalf("restarts = %d, want the import and delete each to restart once", restarts)
	}
}

// TestRemoveRefusesAFileOutsideTheSourcesDirectory: a hand-edited row must not
// turn delete into an arbitrary file removal.
func TestRemoveRefusesAFileOutsideTheSourcesDirectory(t *testing.T) {
	service, repo, _, _ := newTestService(t)
	outside := writeSource(t, "keep.js", "x")
	repo.seed(domain.LxSource{ID: "a1", Name: "a", FilePath: outside, Enabled: true})

	if err := service.Remove(context.Background(), "a1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a file outside the sources directory was deleted: %v", err)
	}
}

// TestRemoveUnknownID explains a stale delete.
func TestRemoveUnknownID(t *testing.T) {
	service, _, _, _ := newTestService(t)
	if err := service.Remove(context.Background(), "ghost"); !errors.Is(err, domain.ErrLxSourceNotFound) {
		t.Fatalf("Remove(ghost) = %v, want ErrLxSourceNotFound", err)
	}
}

// TestReorderAppliesTheNewOrder is acceptance case 9: reorder persists the new
// try order and restarts the plugin.
func TestReorderAppliesTheNewOrder(t *testing.T) {
	service, repo, registry, _ := newTestService(t)
	repo.seed(
		domain.LxSource{ID: "a", Name: "A", FilePath: "/a.js", Enabled: true, Position: 0},
		domain.LxSource{ID: "b", Name: "B", FilePath: "/b.js", Enabled: true, Position: 1},
		domain.LxSource{ID: "c", Name: "C", FilePath: "/c.js", Enabled: true, Position: 2},
	)

	if err := service.Reorder(context.Background(), []string{"c", "a", "b"}); err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	stored, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for index, want := range []string{"c", "a", "b"} {
		if stored[index].ID != want {
			t.Fatalf("position %d = %s, want %s", index, stored[index].ID, want)
		}
	}
	if _, _, restarts, _ := registry.counts(); restarts != 1 {
		t.Fatalf("restarts = %d, want one restart after a reorder", restarts)
	}
}

// TestLaunchEnvCarriesTheEnabledSourcesInOrder is the core half of task §3: the
// environment variable is a JSON array of enabled sources, ordered by position,
// with disabled and blank entries omitted.
func TestLaunchEnvCarriesTheEnabledSourcesInOrder(t *testing.T) {
	service, repo, _, _ := newTestService(t)
	repo.seed(
		domain.LxSource{ID: "b", Name: "B", FilePath: "/b.js", Enabled: true, Position: 1},
		domain.LxSource{ID: "a", Name: "A", FilePath: "/a.js", Enabled: true, Position: 0},
		domain.LxSource{ID: "off", Name: "off", FilePath: "/off.js", Enabled: false, Position: 2},
	)

	env, err := service.LaunchEnv(context.Background())
	if err != nil {
		t.Fatalf("LaunchEnv: %v", err)
	}
	if len(env) != 1 {
		t.Fatalf("LaunchEnv = %v, want one assignment", env)
	}
	key, value, ok := strings.Cut(env[0], "=")
	if !ok || key != SourcesEnvVar {
		t.Fatalf("LaunchEnv = %q, want %s=...", env[0], SourcesEnvVar)
	}

	var entries []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Path     string `json:"path"`
		Enabled  bool   `json:"enabled"`
		Position int    `json:"position"`
	}
	if err := json.Unmarshal([]byte(value), &entries); err != nil {
		t.Fatalf("decode LaunchEnv: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want exactly the two enabled sources", entries)
	}
	if entries[0].ID != "a" || entries[1].ID != "b" {
		t.Fatalf("order = %s,%s; want position order a,b", entries[0].ID, entries[1].ID)
	}
	for _, entry := range entries {
		if !entry.Enabled {
			t.Fatalf("%s is in the list but marked disabled", entry.ID)
		}
		if entry.Position != 0 && entry.Position != 1 {
			t.Fatalf("%s position = %d", entry.ID, entry.Position)
		}
	}
}

// TestLaunchEnvIsEmptyJSONWithNoSources: the variable is always present, so the
// plugin can tell "no sources" from "the host forgot to inject anything".
func TestLaunchEnvIsEmptyJSONWithNoSources(t *testing.T) {
	service, _, _, _ := newTestService(t)
	env, err := service.LaunchEnv(context.Background())
	if err != nil {
		t.Fatalf("LaunchEnv: %v", err)
	}
	if len(env) != 1 || env[0] != SourcesEnvVar+"=[]" {
		t.Fatalf("LaunchEnv = %v, want the empty list", env)
	}
}

// TestDescribeExplainsAStoppedPlugin: the page must be able to say why it cannot
// show platforms, instead of rendering an empty list as "every source is broken".
func TestDescribeExplainsAStoppedPlugin(t *testing.T) {
	service, _, registry, _ := newTestService(t)
	registry.setRunning(false)
	_, err := service.Describe(context.Background())
	if !errors.Is(err, domain.ErrLxPluginUnavailable) {
		t.Fatalf("Describe = %v, want ErrLxPluginUnavailable", err)
	}
	if !strings.Contains(err.Error(), "not running") {
		t.Fatalf("Describe error = %q, want it to name the state", err)
	}
}

// TestDescribeReturnsThePluginAnswer keeps the join honest: the service does not
// invent providers, it passes the plugin's answer through.
func TestDescribeReturnsThePluginAnswer(t *testing.T) {
	service, _, registry, _ := newTestService(t)
	registry.describe = domain.PluginDescribe{
		Providers: []string{"wy", "kg"},
		Sources:   []domain.PluginSourceDescription{{ID: "a1", Name: "ikun", Enabled: true, Providers: []string{"wy"}}},
	}
	describe, err := service.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(describe.Providers) != 2 || describe.Providers[0] != "wy" {
		t.Fatalf("Describe = %+v, want the plugin's answer", describe)
	}
}

// TestListIsEmptyNotNil keeps the JSON contract stable: an empty list must encode
// as [] so the frontend can map over it without a nil check.
func TestListIsEmptyNotNil(t *testing.T) {
	service, _, _, _ := newTestService(t)
	sources, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if sources == nil {
		t.Fatal("List must return an empty slice, not nil")
	}
}

// TestWithinDir: the guard that keeps a hand-edited row from deleting arbitrary
// files must accept the copy and reject traversal, prefixes and bare parents.
func TestWithinDir(t *testing.T) {
	dir := filepath.Join(string(filepath.Separator), "data", "sources")
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(dir, "a1.js"), true},
		{filepath.Join(dir, "..", "airmedy.db"), false},
		{dir, false},
		{filepath.Join(dir, "..", "sources-evil", "x.js"), false},
		{string(filepath.Separator) + "elsewhere.js", false},
	}
	for _, testCase := range cases {
		if got := withinDir(testCase.path, dir); got != testCase.want {
			t.Fatalf("withinDir(%q) = %v, want %v", testCase.path, got, testCase.want)
		}
	}
}
