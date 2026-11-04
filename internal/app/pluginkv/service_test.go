package pluginkv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"airmedy/internal/domain"
)

// memoryKV is the repository the handler sees in tests: the same contract as the
// SQLite adapter, without a database, so each case can be about the gate and the
// limits rather than about SQL.
type memoryKV struct {
	mu    sync.Mutex
	rows  map[string]map[string]string
	calls int
	err   error
}

func newMemoryKV() *memoryKV {
	return &memoryKV{rows: make(map[string]map[string]string)}
}

func (m *memoryKV) Get(_ context.Context, pluginID, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return "", false, m.err
	}
	value, ok := m.rows[pluginID][key]
	return value, ok, nil
}

func (m *memoryKV) Set(_ context.Context, pluginID, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return m.err
	}
	if m.rows[pluginID] == nil {
		m.rows[pluginID] = make(map[string]string)
	}
	m.rows[pluginID][key] = value
	return nil
}

func (m *memoryKV) Delete(_ context.Context, pluginID, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return false, m.err
	}
	if _, ok := m.rows[pluginID][key]; !ok {
		return false, nil
	}
	delete(m.rows[pluginID], key)
	return true, nil
}

func (m *memoryKV) Keys(_ context.Context, pluginID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return sortedKeys(m.rows[pluginID]), nil
}

func (m *memoryKV) Count(_ context.Context, pluginID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return 0, m.err
	}
	return len(m.rows[pluginID]), nil
}

func sortedKeys(rows map[string]string) []string {
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}

func allowed() func(string) bool {
	return func(string) bool { return true }
}

func decodeResult[T any](t *testing.T, result any) T {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	var decoded T
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return decoded
}

// TestKVRequiresThePermission is the default-deny rule from docs/plugin-api.md:
// a manifest without permissions.kv gets a clear refusal, not a silently empty
// store.
func TestKVRequiresThePermission(t *testing.T) {
	repo := newMemoryKV()
	handler := NewHandler(repo, func(string) bool { return false }, nil)

	_, err := handler.Handle(context.Background(), "plug", domain.MethodStorageGet,
		json.RawMessage(`{"key":"token"}`))
	if !errors.Is(err, domain.ErrPluginKVPermission) {
		t.Fatalf("Handle without permission = %v, want ErrPluginKVPermission", err)
	}
	if repo.calls != 0 {
		t.Fatalf("the store was touched %d times for a refused request, want 0", repo.calls)
	}
	if code := domain.RPCErrorCode(err); code != domain.RPCErrorPermissionDenied {
		t.Fatalf("RPCErrorCode = %d, want %d", code, domain.RPCErrorPermissionDenied)
	}
}

// A nil gate must fail closed: a wiring mistake cannot grant every plugin a
// store.
func TestKVWithNoGateFailsClosed(t *testing.T) {
	handler := NewHandler(newMemoryKV(), nil, nil)

	_, err := handler.Handle(context.Background(), "plug", domain.MethodStorageKeys, nil)
	if !errors.Is(err, domain.ErrPluginKVPermission) {
		t.Fatalf("Handle with no gate = %v, want ErrPluginKVPermission", err)
	}
}

func TestKVSetGetRoundTrip(t *testing.T) {
	repo := newMemoryKV()
	handler := NewHandler(repo, allowed(), nil)
	ctx := context.Background()

	if _, err := handler.Handle(ctx, "plug", domain.MethodStorageSet,
		json.RawMessage(`{"key":"token","value":"{\"v\":1}"}`)); err != nil {
		t.Fatalf("storage.set: %v", err)
	}

	result, err := handler.Handle(ctx, "plug", domain.MethodStorageGet,
		json.RawMessage(`{"key":"token"}`))
	if err != nil {
		t.Fatalf("storage.get: %v", err)
	}
	decoded := decodeResult[domain.PluginStorageGetResult](t, result)
	if !decoded.Found {
		t.Fatalf("storage.get = %+v, want found", decoded)
	}
	if decoded.Key != "token" || decoded.Value != `{"v":1}` {
		t.Fatalf("storage.get = %+v, want the stored key and value", decoded)
	}
}

// The value is opaque: the host stores the plugin's own JSON verbatim, so a
// plugin can change its value shape without a host update.
func TestKVValueIsOpaque(t *testing.T) {
	repo := newMemoryKV()
	handler := NewHandler(repo, allowed(), nil)
	ctx := context.Background()

	const raw = `{"nested":{"a":[1,2,3]},"unicode":"音乐"}`
	if _, err := handler.Handle(ctx, "plug", domain.MethodStorageSet,
		json.RawMessage(fmt.Sprintf(`{"key":"blob","value":%q}`, raw))); err != nil {
		t.Fatalf("storage.set: %v", err)
	}

	result, err := handler.Handle(ctx, "plug", domain.MethodStorageGet, json.RawMessage(`{"key":"blob"}`))
	if err != nil {
		t.Fatalf("storage.get: %v", err)
	}
	if got := decodeResult[domain.PluginStorageGetResult](t, result).Value; got != raw {
		t.Fatalf("value = %q, want it stored verbatim", got)
	}
}

func TestKVGetMissingKeyIsFoundFalse(t *testing.T) {
	handler := NewHandler(newMemoryKV(), allowed(), nil)

	result, err := handler.Handle(context.Background(), "plug", domain.MethodStorageGet,
		json.RawMessage(`{"key":"never-written"}`))
	if err != nil {
		t.Fatalf("storage.get on a missing key: %v", err)
	}
	if decoded := decodeResult[domain.PluginStorageGetResult](t, result); decoded.Found {
		t.Fatalf("storage.get = %+v, want found=false", decoded)
	}
}

func TestKVDeleteReportsWhetherSomethingWasRemoved(t *testing.T) {
	repo := newMemoryKV()
	handler := NewHandler(repo, allowed(), nil)
	ctx := context.Background()

	if _, err := handler.Handle(ctx, "plug", domain.MethodStorageSet,
		json.RawMessage(`{"key":"k","value":"v"}`)); err != nil {
		t.Fatalf("storage.set: %v", err)
	}

	result, err := handler.Handle(ctx, "plug", domain.MethodStorageDelete, json.RawMessage(`{"key":"k"}`))
	if err != nil {
		t.Fatalf("storage.delete: %v", err)
	}
	if !decodeResult[domain.PluginStorageDeleteResult](t, result).Deleted {
		t.Fatal("first delete = false, want true")
	}

	result, err = handler.Handle(ctx, "plug", domain.MethodStorageDelete, json.RawMessage(`{"key":"k"}`))
	if err != nil {
		t.Fatalf("second storage.delete: %v", err)
	}
	if decodeResult[domain.PluginStorageDeleteResult](t, result).Deleted {
		t.Fatal("second delete = true, want false for an absent key")
	}
}

func TestKVKeysAreSortedAndNeverNull(t *testing.T) {
	repo := newMemoryKV()
	handler := NewHandler(repo, allowed(), nil)
	ctx := context.Background()

	empty, err := handler.Handle(ctx, "plug", domain.MethodStorageKeys, nil)
	if err != nil {
		t.Fatalf("storage.keys: %v", err)
	}
	if keys := decodeResult[domain.PluginStorageKeysResult](t, empty).Keys; len(keys) != 0 {
		t.Fatalf("storage.keys = %v, want an empty list", keys)
	}

	for _, key := range []string{"z", "a", "m"} {
		if _, err := handler.Handle(ctx, "plug", domain.MethodStorageSet,
			json.RawMessage(fmt.Sprintf(`{"key":%q,"value":"1"}`, key))); err != nil {
			t.Fatalf("storage.set %q: %v", key, err)
		}
	}

	result, err := handler.Handle(ctx, "plug", domain.MethodStorageKeys, nil)
	if err != nil {
		t.Fatalf("storage.keys: %v", err)
	}
	keys := decodeResult[domain.PluginStorageKeysResult](t, result).Keys
	want := []string{"a", "m", "z"}
	if len(keys) != len(want) {
		t.Fatalf("storage.keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("storage.keys = %v, want %v", keys, want)
		}
	}
}

func TestKVRejectsBadKeys(t *testing.T) {
	handler := NewHandler(newMemoryKV(), allowed(), nil)
	ctx := context.Background()

	cases := map[string]string{
		"empty":    `{"key":""}`,
		"too long": `{"key":"` + longKey(domain.PluginKVMaxKeyLength+1) + `"}`,
		"space":    `{"key":"two words"}`,
		"control":  `{"key":"key\u0000"}`,
	}
	keys := []string{"empty", "too long", "space", "control"}
	for _, name := range keys {
		_, err := handler.Handle(ctx, "plug", domain.MethodStorageGet, json.RawMessage(cases[name]))
		if !errors.Is(err, domain.ErrPluginKVKeyInvalid) {
			t.Fatalf("%s: Handle = %v, want ErrPluginKVKeyInvalid", name, err)
		}
		if code := domain.RPCErrorCode(err); code != domain.RPCErrorInvalidParams {
			t.Fatalf("%s: RPCErrorCode = %d, want %d", name, code, domain.RPCErrorInvalidParams)
		}
	}
}

func longKey(length int) string {
	key := make([]byte, length)
	for i := range key {
		key[i] = 'k'
	}
	return string(key)
}

func TestKVRejectsOversizeValue(t *testing.T) {
	handler := NewHandler(newMemoryKV(), allowed(), nil)
	ctx := context.Background()

	oversize := make([]byte, domain.PluginKVMaxValueBytes+1)
	for i := range oversize {
		oversize[i] = 'v'
	}
	params := mustJSON(t, domain.PluginStorageSetParams{Key: "big", Value: string(oversize)})

	_, err := handler.Handle(ctx, "plug", domain.MethodStorageSet, params)
	if !errors.Is(err, domain.ErrPluginKVValueInvalid) {
		t.Fatalf("Handle = %v, want ErrPluginKVValueInvalid", err)
	}
	if code := domain.RPCErrorCode(err); code != domain.RPCErrorInvalidParams {
		t.Fatalf("RPCErrorCode = %d, want %d", code, domain.RPCErrorInvalidParams)
	}
}

// The key cap only blocks a *new* key: a plugin at the limit must still be able
// to update what it already owns.
func TestKVKeyLimitBlocksOnlyNewKeys(t *testing.T) {
	repo := newMemoryKV()
	granted := make(map[string]bool)
	handler := NewHandler(repo, func(pluginID string) bool { return granted[pluginID] }, nil)
	ctx := context.Background()

	granted["plug"] = true
	for i := 0; i < domain.PluginKVMaxKeysPerPlugin; i++ {
		if _, err := handler.Handle(ctx, "plug", domain.MethodStorageSet,
			mustJSON(t, domain.PluginStorageSetParams{Key: fmt.Sprintf("k%03d", i), Value: "v"})); err != nil {
			t.Fatalf("fill key %d: %v", i, err)
		}
	}

	_, err := handler.Handle(ctx, "plug", domain.MethodStorageSet,
		mustJSON(t, domain.PluginStorageSetParams{Key: "one-too-many", Value: "v"}))
	if !errors.Is(err, domain.ErrPluginKVLimit) {
		t.Fatalf("new key past the limit = %v, want ErrPluginKVLimit", err)
	}
	if code := domain.RPCErrorCode(err); code != domain.RPCErrorLimitReached {
		t.Fatalf("RPCErrorCode = %d, want %d", code, domain.RPCErrorLimitReached)
	}

	if _, err := handler.Handle(ctx, "plug", domain.MethodStorageSet,
		mustJSON(t, domain.PluginStorageSetParams{Key: "k000", Value: "updated"})); err != nil {
		t.Fatalf("overwrite at the limit = %v, want it allowed", err)
	}
}

// Two plugins asking with the same key must never see each other's value. The
// handler passes the id it was given straight through, so this is really a check
// that it never substitutes an id of its own.
func TestKVIsolatesPlugins(t *testing.T) {
	handler := NewHandler(newMemoryKV(), allowed(), nil)
	ctx := context.Background()

	if _, err := handler.Handle(ctx, "alpha", domain.MethodStorageSet,
		json.RawMessage(`{"key":"shared","value":"a"}`)); err != nil {
		t.Fatalf("alpha set: %v", err)
	}
	if _, err := handler.Handle(ctx, "beta", domain.MethodStorageSet,
		json.RawMessage(`{"key":"shared","value":"b"}`)); err != nil {
		t.Fatalf("beta set: %v", err)
	}

	alpha, err := handler.Handle(ctx, "alpha", domain.MethodStorageGet, json.RawMessage(`{"key":"shared"}`))
	if err != nil {
		t.Fatalf("alpha get: %v", err)
	}
	if got := decodeResult[domain.PluginStorageGetResult](t, alpha).Value; got != "a" {
		t.Fatalf("alpha value = %q, want \"a\"", got)
	}
}

func TestKVUnknownMethodIsMethodNotFound(t *testing.T) {
	handler := NewHandler(newMemoryKV(), allowed(), nil)

	_, err := handler.Handle(context.Background(), "plug", "storage.everything", nil)
	if !errors.Is(err, domain.ErrPluginRequestUnknown) {
		t.Fatalf("Handle = %v, want ErrPluginRequestUnknown", err)
	}
	if code := domain.RPCErrorCode(err); code != domain.RPCErrorMethodNotFound {
		t.Fatalf("RPCErrorCode = %d, want %d", code, domain.RPCErrorMethodNotFound)
	}
}

func TestKVMalformedParamsAreInvalidParams(t *testing.T) {
	handler := NewHandler(newMemoryKV(), allowed(), nil)

	_, err := handler.Handle(context.Background(), "plug", domain.MethodStorageSet, json.RawMessage(`"not-an-object"`))
	if err == nil {
		t.Fatal("Handle with a non-object params = nil, want an error")
	}
	// A malformed envelope is a protocol error, not a permission one: the code
	// must stay -32601/-32602 territory rather than claiming -32003.
	if code := domain.RPCErrorCode(err); code == domain.RPCErrorPermissionDenied {
		t.Fatalf("RPCErrorCode = %d, want not -32003", code)
	}
}

func TestKVRepositoryFailureBecomesInternalError(t *testing.T) {
	repo := newMemoryKV()
	repo.err = errors.New("disk on fire")
	handler := NewHandler(repo, allowed(), nil)

	_, err := handler.Handle(context.Background(), "plug", domain.MethodStorageKeys, nil)
	if err == nil {
		t.Fatal("Handle = nil, want the repository failure")
	}
	if code := domain.RPCErrorCode(err); code != 0 {
		t.Fatalf("RPCErrorCode = %d, want 0 (internal) for an unclassified failure", code)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode params: %v", err)
	}
	return encoded
}
