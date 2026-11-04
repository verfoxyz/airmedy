package pluginkv

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"airmedy/internal/domain"
)

// Handler serves the plugin → host request set (storage.*) for one plugin.
//
// It is the host side of the first inbound methods: until now every plugin
// request was answered -32601 because the host exposed nothing (see
// catalog/plugins/README.md, "Wire protocol"). KV is the first thing a plugin
// genuinely cannot own itself — a child process has no persistence, so the
// store lives here, keyed by the plugin id the manifest declares.
//
// The handler is deliberately tiny and dependency-free: it owns the permission
// gate, the limits, and the wire shapes, and nothing else. It sits in domain's
// neighbour layer (app) because it is a use case; the transport calls it through
// a function value, so infra keeps no import of app (see AGENTS.md §3).
type Handler struct {
	repo   domain.PluginKVRepository
	logger *slog.Logger
	// permission reports whether pluginID declared `permissions.kv`.
	//
	// It is attached after construction rather than injected: the plugin
	// registry consumes the host factory, which carries this handler, so a
	// constructor argument from the registry would close a dependency cycle
	// (main.go's fx graph rejects it). AttachPermission is the same
	// attach-after-construction edge app/lx uses for its registry.
	permissionMu sync.RWMutex
	permission   func(pluginID string) bool
}

// NewHandler builds the storage.* handler. permission may be nil, in which case
// every request is refused: KV is opt-in per plugin (`permissions.kv`), and a
// missing gate must fail closed.
func NewHandler(repo domain.PluginKVRepository, permission func(pluginID string) bool, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{repo: repo, permission: permission, logger: logger}
}

// AttachPermission binds the permission table after construction.
//
// A nil table keeps the default, which is to refuse: a wiring mistake must not
// hand every plugin a store. It is safe to call once, before plugins start
// (fx.Invoke runs before the lifecycle's OnStart).
func (h *Handler) AttachPermission(permission func(pluginID string) bool) {
	h.permissionMu.Lock()
	defer h.permissionMu.Unlock()
	h.permission = permission
}

// Handle answers one inbound request. A nil error means the request was served;
// result holds the answer object, which the transport encodes as `result`.
//
// Unknown methods return ErrPluginRequestUnknown so the transport can answer the
// protocol's -32601, which is what an older plugin and an older host have always
// agreed on.
func (h *Handler) Handle(ctx context.Context, pluginID, method string, params json.RawMessage) (any, error) {
	if !h.allowed(pluginID) {
		return nil, fmt.Errorf("%w: plugin %q did not declare permissions.kv", domain.ErrPluginKVPermission, pluginID)
	}

	switch method {
	case domain.MethodStorageGet:
		return h.get(ctx, pluginID, params)
	case domain.MethodStorageSet:
		return h.set(ctx, pluginID, params)
	case domain.MethodStorageDelete:
		return h.delete(ctx, pluginID, params)
	case domain.MethodStorageKeys:
		return h.keys(ctx, pluginID)
	default:
		return nil, fmt.Errorf("%w: %q", domain.ErrPluginRequestUnknown, method)
	}
}

// allowed applies the default-deny permission rule.
func (h *Handler) allowed(pluginID string) bool {
	permission := h.currentPermission()
	if permission == nil || pluginID == "" {
		return false
	}
	return permission(pluginID)
}

func (h *Handler) currentPermission() func(string) bool {
	h.permissionMu.RLock()
	defer h.permissionMu.RUnlock()
	return h.permission
}

func (h *Handler) get(ctx context.Context, pluginID string, params json.RawMessage) (any, error) {
	var request domain.PluginStorageKeyParams
	if err := decodeParams(params, &request); err != nil {
		return nil, err
	}
	if err := domain.ValidatePluginKVKey(request.Key); err != nil {
		return nil, err
	}

	value, found, err := h.repo.Get(ctx, pluginID, request.Key)
	if err != nil {
		return nil, fmt.Errorf("read plugin kv: %w", err)
	}
	return domain.PluginStorageGetResult{Key: request.Key, Value: value, Found: found}, nil
}

func (h *Handler) set(ctx context.Context, pluginID string, params json.RawMessage) (any, error) {
	var request domain.PluginStorageSetParams
	if err := decodeParams(params, &request); err != nil {
		return nil, err
	}
	if err := domain.ValidatePluginKVKey(request.Key); err != nil {
		return nil, err
	}
	if err := domain.ValidatePluginKVValue(request.Value); err != nil {
		return nil, err
	}

	// The key cap only applies to a *new* key: overwriting an existing one
	// cannot grow the table, and refusing that would make a plugin that is at
	// the limit unable to update what it already owns.
	_, found, err := h.repo.Get(ctx, pluginID, request.Key)
	if err != nil {
		return nil, fmt.Errorf("read plugin kv: %w", err)
	}
	if !found {
		count, err := h.repo.Count(ctx, pluginID)
		if err != nil {
			return nil, fmt.Errorf("count plugin kv: %w", err)
		}
		if count >= domain.PluginKVMaxKeysPerPlugin {
			return nil, fmt.Errorf("%w: plugin %q already stores %d keys (limit %d)",
				domain.ErrPluginKVLimit, pluginID, count, domain.PluginKVMaxKeysPerPlugin)
		}
	}

	if err := h.repo.Set(ctx, pluginID, request.Key, request.Value); err != nil {
		return nil, fmt.Errorf("write plugin kv: %w", err)
	}
	// storage.set has no payload: a plugin that wants to read back what it wrote
	// calls storage.get, which keeps the answer shape from carrying the value
	// twice.
	return struct{}{}, nil
}

func (h *Handler) delete(ctx context.Context, pluginID string, params json.RawMessage) (any, error) {
	var request domain.PluginStorageKeyParams
	if err := decodeParams(params, &request); err != nil {
		return nil, err
	}
	if err := domain.ValidatePluginKVKey(request.Key); err != nil {
		return nil, err
	}

	deleted, err := h.repo.Delete(ctx, pluginID, request.Key)
	if err != nil {
		return nil, fmt.Errorf("delete plugin kv: %w", err)
	}
	return domain.PluginStorageDeleteResult{Deleted: deleted}, nil
}

func (h *Handler) keys(ctx context.Context, pluginID string) (any, error) {
	keys, err := h.repo.Keys(ctx, pluginID)
	if err != nil {
		return nil, fmt.Errorf("list plugin kv keys: %w", err)
	}
	if keys == nil {
		// An absent key set is an empty list on the wire, not null: a plugin
		// iterating the answer should not need a null check.
		keys = []string{}
	}
	return domain.PluginStorageKeysResult{Keys: keys}, nil
}

// decodeParams decodes a request's params into target. An absent params object
// is treated as an empty one, so storage.keys needs no params at all.
func decodeParams(params json.RawMessage, target any) error {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, target); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrPluginRequestParams, err)
	}
	return nil
}
