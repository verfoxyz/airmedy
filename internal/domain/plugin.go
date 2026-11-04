package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// HostAPIVersion is the plugin API version this host implements. It is a
// concrete version, not a range: a manifest declares the range it accepts
// (`"apiVersion": "^1.0"`) and the handshake compares the plugin's concrete
// answer against both that range and this version.
//
// Compatibility policy (see catalog/plugins/README.md, "Wire protocol"): a 1.x host
// only ever adds methods and optional fields, never removes or changes one, so
// a plugin written for an older 1.x minor keeps working and a plugin that
// claims a newer minor does not.
const HostAPIVersion = "1.0"

// RuntimeTypeProcess is the only runtime the v1 host knows: the plugin is a
// child process that speaks JSON-RPC over stdio. Anything else is rejected at
// manifest load time rather than failing later during the handshake.
const RuntimeTypeProcess = "process"

// ManifestFileName is the fixed manifest name looked for by plugin discovery.
const ManifestFileName = "airmedy-plugin.json"

// Capability names one feature a plugin may implement. Values are the wire
// contract, shared with the plugin authors, so they must not be renamed.
type Capability string

const (
	CapabilityAuth Capability = "auth"
	// CapabilityCatalogSearch is the online-catalogue keyword search a plugin
	// can serve (domain.MethodSearch). It is separate from the playlist/daily
	// catalogue capabilities: searching and browsing are different endpoints
	// with different failure modes, and a plugin may support one without the
	// other.
	CapabilityCatalogSearch        Capability = "catalog.search"
	CapabilityCatalogPlaylists     Capability = "catalog.playlists"
	CapabilityCatalogPlaylistItems Capability = "catalog.playlistItems"
	CapabilityCatalogDaily         Capability = "catalog.daily"
	CapabilityStreamResolve        Capability = "stream.resolve"
	CapabilityLyricFetch           Capability = "lyric.fetch"
	CapabilityPicFetch             Capability = "pic.fetch"
	CapabilityUISettingsSchema     Capability = "ui.settingsSchema"
)

// KnownCapabilities lists every capability this host understands today. It is
// a snapshot of the host's knowledge, not a whitelist: see IsKnownCapability.
var KnownCapabilities = []Capability{
	CapabilityAuth,
	CapabilityCatalogSearch,
	CapabilityCatalogPlaylists,
	CapabilityCatalogPlaylistItems,
	CapabilityCatalogDaily,
	CapabilityStreamResolve,
	CapabilityLyricFetch,
	CapabilityPicFetch,
	CapabilityUISettingsSchema,
}

// IsKnownCapability reports whether the host knows how to use c.
//
// Unknown capabilities are not an error. The plugin ecosystem deliberately
// contains forks with extra sources and qualities, and rejecting a whole
// manifest over one string the host has never heard of would lock out working
// plugins for no benefit (tolerant mode, see catalog/plugins/README.md,
// "Wire protocol"). The registry surfaces unknown entries so the UI can mark
// them as non-standard.
func IsKnownCapability(c Capability) bool {
	for _, known := range KnownCapabilities {
		if known == c {
			return true
		}
	}
	return false
}

// HasCapability reports whether one of capabilities is c. A manifest that
// declares providers without stream.resolve cannot route anything; the registry
// rejects that combination (see ErrPluginProviderWithoutCapability).
func HasCapability(capabilities []Capability, c Capability) bool {
	for _, declared := range capabilities {
		if declared == c {
			return true
		}
	}
	return false
}

// PluginManifest is a parsed and validated airmedy-plugin.json.
type PluginManifest struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Version     string        `json:"version"`
	Author      string        `json:"author,omitempty"`
	APIVersion  string        `json:"apiVersion"`
	Runtime     PluginRuntime `json:"runtime"`
	// Capabilities are the features the plugin declares. Unknown values are
	// kept verbatim so the UI can show them without the host pretending to
	// understand them.
	Capabilities []Capability `json:"capabilities"`
	// Providers lists the track provider ids this plugin serves. The app routes
	// a resolve request by this declaration instead of asking every running
	// plugin; empty means the plugin serves no provider (e.g. a lyrics-only
	// plugin).
	//
	// Declaring a provider is a routing contract, not a hint: two plugins
	// claiming the same id are both marked invalid, because the app refuses to
	// guess which one the user meant (see app/plugin's conflict rule).
	Providers []string `json:"providers,omitempty"`
	// Sources lists the online source ids this plugin can serve without
	// producing a playable URL (search and lyrics). It is separate from
	// Providers because a search-only plugin must not claim a stream route.
	// The settings row shows them as tags; lyric routing matches a track's
	// provider id against this list.
	Sources     []string          `json:"sources,omitempty"`
	Permissions PluginPermissions `json:"permissions"`
	// Dir is the directory the manifest was read from; runtime paths resolve
	// relative to it. It is not part of the file.
	Dir string `json:"-"`
}

// PluginRuntime describes how the host launches the plugin.
type PluginRuntime struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// PluginPermissions is the declarative permission block, and it is now split
// between a declaration and an enforced right:
//   - Network is **declared only**. Plugins are ordinary child processes that
//     fetch directly, so the host sees no request and cannot allow or deny one.
//     The UI shows it as "may contact", never "is allowed to" (see
//     catalog/plugins/README.md, "Network: declared, not enforced").
//   - KV is **enforced**: `storage.*` requests are refused unless it is true,
//     in app/pluginkv. It is a host-side capability, so unlike network access
//     there is a real gate to apply.
//
// Both are shown in the settings row before a plugin is enabled.
type PluginPermissions struct {
	Network []string `json:"network,omitempty"`
	KV      bool     `json:"kv,omitempty"`
}

// PluginSettingType is the control a declaration renders. Today only a switch
// exists; the type is a string so an older host can ignore an unknown control
// instead of refusing the whole declaration.
const PluginSettingTypeSwitch = "switch"

// PluginSettingDeclaration is one setting a plugin reports through describe.
//
// The host owns the value (plugin KV) and passes it to the plugin on the call it
// affects; the plugin never reads the KV for this. Label is a locale map so the
// translation travels with the declaration.
type PluginSettingDeclaration struct {
	Key     string `json:"key"`
	Type    string `json:"type"`
	Default bool   `json:"default"`
	// Label maps a locale tag ("en", "zh") to the text. The frontend falls back
	// to Key when its locale is absent.
	Label map[string]string `json:"label,omitempty"`
}

// PluginSettingValue is a declaration plus the value the host currently stores.
type PluginSettingValue struct {
	PluginSettingDeclaration
	Value bool `json:"value"`
}

// MethodLyricFetch is the plugin method that returns lyrics for a track it
// searched. It is separate from stream.resolve: a search plugin can provide
// lyrics without ever producing a playable URL.
const MethodLyricFetch = "lyric.fetch"

// PluginLyricRequest is the params object of lyric.fetch.
type PluginLyricRequest struct {
	Provider   string   `json:"provider"`
	ExternalID string   `json:"external_id"`
	Title      string   `json:"title,omitempty"`
	Artists    []string `json:"artists,omitempty"`
	Album      string   `json:"album,omitempty"`
	Duration   int      `json:"duration,omitempty"`
}

// PluginLyricResult is the result object of lyric.fetch.
//
// Synced is the plugin's own claim; the host still requires a timeline tag
// before storing the lyric, because a plain text body cannot scroll.
type PluginLyricResult struct {
	Lyric  string `json:"lyric"`
	TLyric string `json:"tlyric,omitempty"`
	Source string `json:"source,omitempty"`
	Synced bool   `json:"synced"`
}

// PluginLyricFetcher fetches lyrics through the plugin that owns a track's
// source. A handled=false answer means no running plugin serves that provider,
// so the caller should use the built-in providers.
type PluginLyricFetcher interface {
	FetchForTrack(ctx context.Context, track *TrackDTO) (*Lyric, bool, error)
}

// PluginHost is one running plugin process.
//
// Implementations own the child process and the JSON-RPC session. Call is safe
// for concurrent use; Close is idempotent and unblocks every in-flight call.
type PluginHost interface {
	// Manifest returns the manifest the host was started from.
	Manifest() PluginManifest
	// Capabilities returns what the plugin answered during the handshake.
	Capabilities() []Capability
	// Call invokes one plugin method. params and result are JSON-encoded /
	// decoded; nil params sends no params and nil result discards the answer.
	Call(ctx context.Context, method string, params, result any) error
	// Close asks the plugin to exit, then reclaims the process.
	Close() error
}

// PluginUnloader is the optional half of PluginHost that lets the host tell a
// plugin *why* it is being reclaimed (lifecycle.unload, docs/plugin-api.md).
//
// It is a separate interface rather than a required method so an existing
// implementation (a test fake, an older adapter) still satisfies PluginHost: the
// registry checks for it and falls back to Close. A plugin that does not
// implement the notification is unaffected, because it is a no-id notification
// an unaware plugin simply ignores.
type PluginUnloader interface {
	// Unload sends lifecycle.unload with the reason, gives the plugin its grace
	// to exit by itself, then runs the same escalation Close does.
	Unload(reason PluginUnloadReason) error
}

// PluginLoader reads manifests from disk. infra/pluginhost implements it; the
// app layer only ever sees this port, so it never imports infra.
type PluginLoader interface {
	// Discover returns the manifest paths found under dir, recursively. A
	// missing dir is not an error: it just means no plugins are installed.
	Discover(dir string) ([]string, error)
	// Load parses and validates one manifest file.
	Load(path string) (PluginManifest, error)
}

// PluginHostFactory starts one plugin from an already validated manifest.
//
// It is a function type rather than an interface because it has no state worth
// mocking beyond the call itself: main.go binds infra/pluginhost.Start onto it,
// and tests hand the app layer a closure. This keeps the dependency direction
// infra → app → domain intact while the app service still decides when a plugin
// runs.
type PluginHostFactory func(ctx context.Context, manifest PluginManifest) (PluginHost, error)

// PluginSettings is one persisted enable flag.
//
// A missing row means "not enabled": plugins are off until the user turns them
// on, because enabling one starts a third-party child process.
type PluginSettings struct {
	PluginID  string
	Enabled   bool
	UpdatedAt time.Time
}

// PluginSettingsRepository persists which plugins the user enabled.
//
// It is deliberately a separate port from SettingsRepository: plugin rows are
// keyed by plugin id and appear/disappear as plugins are installed, while
// app_settings is a single fixed row.
type PluginSettingsRepository interface {
	// List returns every persisted row, including rows for plugins that are no
	// longer installed. Callers index by plugin id and ignore unknown ids.
	List(ctx context.Context) ([]PluginSettings, error)
	// Set writes one plugin's flag, inserting the row when absent.
	Set(ctx context.Context, pluginID string, enabled bool) error
}

// PluginStatus is the coarse lifecycle state the UI shows for one plugin.
type PluginStatus string

const (
	// PluginStatusIdle means the plugin is known but not running.
	PluginStatusIdle PluginStatus = "idle"
	// PluginStatusRunning means the plugin process is up and past the
	// handshake.
	PluginStatusRunning PluginStatus = "running"
	// PluginStatusFailed means the last start attempt failed. The reason is
	// carried alongside; the plugin stays registered so one bad plugin never
	// hides the rest.
	PluginStatusFailed PluginStatus = "failed"
)

// JSON-RPC error codes reserved by the plugin protocol (see the v1 contract).
// Standard JSON-RPC codes are used as-is where they fit; the -320xx range is
// the host's own.
const (
	// RPCErrorCrashed marks a plugin process that died mid-call. The host may
	// restart it, so callers should treat the call as retryable.
	RPCErrorCrashed = -32000
	// RPCErrorHandshakeTimeout marks an initialize that never completed in
	// time. The process is killed; retrying the same plugin is pointless.
	RPCErrorHandshakeTimeout = -32001
	// RPCErrorIncompatibleVersion marks an apiVersion the host cannot speak.
	RPCErrorIncompatibleVersion = -32002
	// RPCErrorMethodNotFound is the standard code for an unknown method.
	RPCErrorMethodNotFound = -32601
	// RPCErrorInternal is the standard code for an unexpected host-side
	// failure while handling a request.
	RPCErrorInternal = -32603
	// RPCErrorInvalidParams is the standard code for malformed params (a
	// missing key, a value that is not a string). It is a caller bug, not a
	// host failure, so a plugin should fix the request rather than retry it.
	RPCErrorInvalidParams = -32602
	// RPCErrorPermissionDenied marks an inbound request the plugin's manifest
	// does not allow (for today, `storage.*` without `permissions.kv`). It uses
	// the reserved "server error" range rather than -32601 so a plugin can tell
	// "you did not implement this" from "you may not".
	RPCErrorPermissionDenied = -32003
	// RPCErrorLimitReached marks an inbound request refused because a documented
	// cap would be exceeded (the KV key count). Retrying without deleting
	// something cannot help.
	RPCErrorLimitReached = -32004
)

// PluginRequestHandler answers a plugin → host request.
//
// The transport has always replied -32601 to every inbound method because the
// host exposed none; the first exception is the KV store (domain.PluginKV),
// which a plugin cannot own itself. A handler is bound by main.go through a
// function value so infra keeps no import of app (AGENTS.md §3).
//
// The returned value is encoded as `result` verbatim. Returning an error that
// wraps one of this package's request sentinels maps to the documented JSON-RPC
// code; anything else is -32603.
type PluginRequestHandler func(ctx context.Context, pluginID, method string, params json.RawMessage) (any, error)

// Plugin host errors. Callers use errors.Is to classify a failure without
// parsing messages; the transport maps plugin-supplied JSON-RPC errors onto the
// same sentinels.
var (
	// ErrPluginManifestInvalid marks an unreadable, malformed or incomplete
	// manifest. The plugin is never started.
	ErrPluginManifestInvalid = errors.New("plugin manifest invalid")
	// ErrPluginCrashed marks a plugin process that exited unexpectedly.
	ErrPluginCrashed = errors.New("plugin process crashed")
	// ErrPluginHandshakeTimeout marks an initialize that did not complete.
	ErrPluginHandshakeTimeout = errors.New("plugin handshake timed out")
	// ErrPluginIncompatibleAPIVersion marks a plugin whose API version the
	// host cannot speak. The process is reclaimed; the plugin must not run.
	ErrPluginIncompatibleAPIVersion = errors.New("plugin api version incompatible")
	// ErrPluginMethodNotFound marks a call to a method the plugin does not
	// implement.
	ErrPluginMethodNotFound = errors.New("plugin method not found")
	// ErrPluginHostClosed marks use of a host after Close.
	ErrPluginHostClosed = errors.New("plugin host closed")
	// ErrPluginUnavailable marks a plugin that crashed too often to be
	// restarted again in this session.
	ErrPluginUnavailable = errors.New("plugin unavailable after repeated crashes")
	// ErrPluginNotRunning marks a registry call against a plugin that is not
	// started.
	ErrPluginNotRunning = errors.New("plugin not running")
	// ErrPluginAlreadyRegistered marks a second manifest with the same id.
	ErrPluginAlreadyRegistered = errors.New("plugin already registered")
	// ErrPluginRunningLimit marks a start rejected because the registry
	// already holds the maximum number of running plugin processes.
	ErrPluginRunningLimit = errors.New("plugin running limit reached")
	// ErrPluginNotFound marks a lookup for an unknown plugin id.
	ErrPluginNotFound = errors.New("plugin not found")
	// ErrPluginProviderConflict marks a set of manifests in which the same
	// provider id is declared by more than one plugin. Both plugins are kept
	// but marked invalid: routing by declaration only works when a provider has
	// exactly one owner, so the app refuses to break the tie silently.
	ErrPluginProviderConflict = errors.New("plugin provider conflict")
	// ErrPluginProviderWithoutCapability marks a manifest that declares
	// providers but not the stream.resolve capability. Providers have no
	// meaning without it, so the entry is kept but marked invalid instead of
	// advertising routes nobody can serve.
	ErrPluginProviderWithoutCapability = errors.New("plugin declares providers without stream.resolve")
	// ErrPluginSettingsUnavailable marks a registry whose enabled state cannot
	// be read or written because no settings store is bound. The app still
	// lists plugins; it just cannot persist a toggle.
	ErrPluginSettingsUnavailable = errors.New("plugin settings store unavailable")
)

// RPCError is a JSON-RPC 2.0 error object as returned by a plugin.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements error.
func (e *RPCError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("plugin error %d", e.Code)
	}
	return fmt.Sprintf("plugin error %d: %s", e.Code, e.Message)
}

// Is lets errors.Is classify a plugin error by its documented code without
// every caller switching on the number.
func (e *RPCError) Is(target error) bool {
	switch e.Code {
	case RPCErrorCrashed:
		return target == ErrPluginCrashed
	case RPCErrorHandshakeTimeout:
		return target == ErrPluginHandshakeTimeout
	case RPCErrorIncompatibleVersion:
		return target == ErrPluginIncompatibleAPIVersion
	case RPCErrorMethodNotFound:
		return target == ErrPluginMethodNotFound
	}
	return false
}

// RPCErrorCode returns the JSON-RPC code that belongs to err, or 0 when err is
// not a plugin protocol error. Host-side sentinels map to the code the wire
// contract assigns them so a frontend can show the same number either way.
func RPCErrorCode(err error) int {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Code
	}
	switch {
	case errors.Is(err, ErrPluginCrashed):
		return RPCErrorCrashed
	case errors.Is(err, ErrPluginHandshakeTimeout):
		return RPCErrorHandshakeTimeout
	case errors.Is(err, ErrPluginIncompatibleAPIVersion):
		return RPCErrorIncompatibleVersion
	case errors.Is(err, ErrPluginMethodNotFound):
		return RPCErrorMethodNotFound
	case errors.Is(err, ErrPluginRequestUnknown):
		return RPCErrorMethodNotFound
	case errors.Is(err, ErrPluginRequestParams):
		return RPCErrorInvalidParams
	case errors.Is(err, ErrPluginKVPermission):
		return RPCErrorPermissionDenied
	case errors.Is(err, ErrPluginKVKeyInvalid), errors.Is(err, ErrPluginKVValueInvalid):
		return RPCErrorInvalidParams
	case errors.Is(err, ErrPluginKVLimit):
		return RPCErrorLimitReached
	}
	return 0
}

// MethodStreamResolve is the plugin method that turns a remote track into a
// directly playable URL. It is the first capability wired into a host feature;
// the plugin resolves, the host downloads (see catalog/download/README.md).
const MethodStreamResolve = "stream.resolve"

// MethodDescribe is the optional plugin method that reports its current routes
// and sub-sources.
//
// A plugin whose content the user changes at runtime cannot declare its
// providers statically: the lx plugin loads scripts the user imported, so its
// manifest lists no providers and the real set is only known after the scripts
// ran. The host calls describe after initialize (and after every restart of a
// dynamic plugin) and adopts the answer as the routing table.
//
// A plugin that does not implement it answers -32601, which the registry treats
// as "static manifest only" rather than as a failure.
const MethodDescribe = "describe"

// Plugin stream error kinds. A plugin reports a failure kind in the JSON-RPC
// error's `data` object, never by wording its message: the host maps kinds to
// domain sentinels so retry policy is not hostage to free text.
const (
	// StreamErrorRateLimited means the source asked the plugin to slow down;
	// the download service backs off and retries.
	StreamErrorRateLimited = "rate_limited"
	// StreamErrorUnavailable means the track cannot be played from this source
	// (copyright, withdrawn, no URL). Retrying cannot help.
	StreamErrorUnavailable = "unavailable"
	// StreamErrorVIP marks a paid/VIP-only track. It behaves like unavailable
	// (no retry) but is kept apart so the UI can explain why.
	StreamErrorVIP = "vip"
	// StreamErrorAuth means the plugin's credentials were rejected. A retry
	// with the same state cannot help.
	StreamErrorAuth = "auth"
)

// PluginStreamMetadata is the optional hint the host passes alongside the
// source id. Plugins that only accept platform ids must ignore it; plugins that
// search by text use it as the fallback.
type PluginStreamMetadata struct {
	Title    string   `json:"title,omitempty"`
	Artists  []string `json:"artists,omitempty"`
	Album    string   `json:"album,omitempty"`
	Duration int      `json:"duration,omitempty"` // seconds
}

// PluginStreamRequest is the params object of stream.resolve.
//
// Source and ID are required: a real audio-source script only accepts the
// platform id pair and cannot resolve from metadata alone.
type PluginStreamRequest struct {
	Source   string               `json:"source"`
	ID       string               `json:"id"`
	Quality  string               `json:"quality"`
	Metadata PluginStreamMetadata `json:"metadata"`
}

// PluginStreamResult is the result object of stream.resolve.
//
// ExpiresAt is an advisory RFC 3339 timestamp, not a cache instruction: the
// download service re-resolves before every attempt, and a malformed hint must
// not turn a playable URL into a failure.
type PluginStreamResult struct {
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	ExpiresAt string            `json:"expiresAt,omitempty"`
}

// PluginErrorData is the structured `error.data` object a plugin may attach to
// a stream.resolve failure. Kind is one of the StreamError* constants.
type PluginErrorData struct {
	Kind string `json:"kind,omitempty"`
}
