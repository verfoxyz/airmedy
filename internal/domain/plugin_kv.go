package domain

import (
	"context"
	"errors"
	"fmt"
)

// Plugin KV storage (docs/plugin-api.md, "第一批"): a plugin's own key/value
// space, owned and persisted by the host.
//
// Why the host owns it: a plugin is a child process with no persistence, so
// whatever it wants to remember (a cached token, a user-chosen source, a resume
// position) would otherwise die on every restart or plugin update. Putting the
// store in the core also makes the data inspectable and isolated per plugin by
// construction rather than by convention.
//
// The other half of the rule: a plugin never reaches the database directly —
// there is no SQLite handle in its process, and the only persistence it gets is
// this KV.
const (
	// PluginKVMaxValueBytes bounds one stored value. KV is for a plugin's own
	// bookkeeping, not for bulk data: an lx source list lives in `lx_sources`,
	// artwork lives in the artwork store. Enforced host-side so a buggy plugin
	// cannot grow the database without a bound.
	PluginKVMaxValueBytes = 64 << 10 // 64 KiB
	// PluginKVMaxKeyLength bounds one key. Keys are opaque plugin-chosen
	// strings, not paths, and a bound keeps them usable in a UI listing and in
	// a log line.
	PluginKVMaxKeyLength = 128
	// PluginKVMaxKeysPerPlugin bounds how many keys one plugin may store.
	// Without it a plugin writing a key per track turns KV into an unbounded
	// table — which is a different feature, and one that belongs behind a
	// dedicated repository port.
	PluginKVMaxKeysPerPlugin = 256
)

// Method names of the plugin → host request set. These are the first methods the
// host *answers*; every other inbound method is still answered -32601.
//
// They sit under `storage.` because that is the capability, not because they
// touch a database: the host owns the bytes, the plugin chooses the shape.
const (
	// MethodStorageGet reads one value: {key} → {key, value, found}.
	MethodStorageGet = "storage.get"
	// MethodStorageSet writes one value: {key, value} → {}.
	MethodStorageSet = "storage.set"
	// MethodStorageDelete removes one key: {key} → {deleted}. Deleting a
	// missing key is not an error, so a plugin can clean up unconditionally.
	MethodStorageDelete = "storage.delete"
	// MethodStorageKeys lists the plugin's keys, sorted: {} → {keys}.
	MethodStorageKeys = "storage.keys"
	// MethodLifecycleUnload is the host → plugin notification sent before the
	// process is reclaimed: {reason}. It is a notification (no id), so an
	// unaware plugin simply ignores it and behaves as before.
	MethodLifecycleUnload = "lifecycle.unload"
)

// PluginUnloadReason is why the host is asking a plugin to release its state.
//
// A plugin needs this to tell "the user turned me off" from "the app is
// quitting": the first case should tear everything down, the second may want to
// flush something first. Both end with the process exiting.
type PluginUnloadReason string

const (
	// PluginUnloadReasonDisabled means the plugin was switched off, including
	// the stop half of a restart the user caused.
	PluginUnloadReasonDisabled PluginUnloadReason = "disabled"
	// PluginUnloadReasonShutdown means the application is exiting.
	PluginUnloadReasonShutdown PluginUnloadReason = "shutdown"
	// PluginUnloadReasonReload means the host restarted the plugin on purpose
	// (source list change, settings edit). The process ends either way; the
	// distinction only changes what a plugin flushes first.
	PluginUnloadReasonReload PluginUnloadReason = "reload"
)

// ValidatePluginKVKey checks a plugin-chosen key.
//
// Keys are opaque but printable: no whitespace, no control characters, bounded
// length. A plugin is free to choose `token`, `cache/last` or `v1.settings`; it
// is not free to choose a key that cannot be rendered in the UI or one line of
// a log.
func ValidatePluginKVKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: key is empty", ErrPluginKVKeyInvalid)
	}
	if len(key) > PluginKVMaxKeyLength {
		return fmt.Errorf("%w: key is longer than %d bytes", ErrPluginKVKeyInvalid, PluginKVMaxKeyLength)
	}
	for _, char := range key {
		if char < 0x20 || char == 0x7f {
			return fmt.Errorf("%w: key contains a control character", ErrPluginKVKeyInvalid)
		}
		if char == ' ' {
			return fmt.Errorf("%w: key contains a space", ErrPluginKVKeyInvalid)
		}
	}
	return nil
}

// ValidatePluginKVValue checks a value against PluginKVMaxValueBytes.
func ValidatePluginKVValue(value string) error {
	if len(value) > PluginKVMaxValueBytes {
		return fmt.Errorf("%w: value is %d bytes, the limit is %d",
			ErrPluginKVValueInvalid, len(value), PluginKVMaxValueBytes)
	}
	return nil
}

// PluginKVRepository persists plugin key/value pairs.
//
// It has no notion of "the current plugin": the caller passes the id it serves,
// which is how isolation lives in one place instead of in every query.
type PluginKVRepository interface {
	// Get returns one value. found is false when the key is absent; a missing
	// key is not an error.
	Get(ctx context.Context, pluginID, key string) (value string, found bool, err error)
	// Set writes one value, inserting the row when absent.
	Set(ctx context.Context, pluginID, key, value string) error
	// Delete removes one key and reports whether a row was removed. Deleting a
	// missing key is not an error.
	Delete(ctx context.Context, pluginID, key string) (deleted bool, err error)
	// Keys lists the plugin's keys in ascending order.
	Keys(ctx context.Context, pluginID string) ([]string, error)
	// Count returns how many keys the plugin stores, so the caller can enforce
	// PluginKVMaxKeysPerPlugin without loading values.
	Count(ctx context.Context, pluginID string) (int, error)
}

// The wire shapes of storage.* are separate from the repository on purpose: the
// contract is published to plugin authors, so `value` travels as a string
// carrying the plugin's own JSON, and `found` / `deleted` exist so a plugin
// never has to distinguish "null" from "absent" by guessing.

// PluginStorageGetResult is the result object of storage.get.
type PluginStorageGetResult struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Found bool   `json:"found"`
}

// PluginStorageSetParams is the params object of storage.set.
type PluginStorageSetParams struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// PluginStorageKeyParams is the params object of the single-key methods.
type PluginStorageKeyParams struct {
	Key string `json:"key"`
}

// PluginStorageKeysResult is the result object of storage.keys.
type PluginStorageKeysResult struct {
	Keys []string `json:"keys"`
}

// PluginStorageDeleteResult is the result object of storage.delete.
//
// Deleted is an answer rather than an error because removing an absent key is a
// normal cleanup path, and a plugin forced to special-case -32004 for it would
// end up ignoring all errors instead.
type PluginStorageDeleteResult struct {
	Deleted bool `json:"deleted"`
}

// PluginUnloadParams is the params object of the lifecycle.unload notification.
type PluginUnloadParams struct {
	Reason PluginUnloadReason `json:"reason"`
}

// Host-side refusals of an inbound plugin request.
//
// These are distinct from a plugin's own failure kinds: they describe the host
// declining to serve a request, and a plugin should treat all of them as
// "do not retry, degrade gracefully".
var (
	// ErrPluginKVKeyInvalid marks a key the host will not store.
	ErrPluginKVKeyInvalid = errors.New("plugin kv key invalid")
	// ErrPluginKVValueInvalid marks a value over the byte cap.
	ErrPluginKVValueInvalid = errors.New("plugin kv value invalid")
	// ErrPluginKVLimit marks a set that would exceed the per-plugin key count.
	ErrPluginKVLimit = errors.New("plugin kv limit reached")
	// ErrPluginKVPermission marks a storage.* request from a plugin whose
	// manifest does not declare `permissions.kv`. The default is deny, so a
	// plugin that forgets the declaration gets a clear refusal instead of an
	// empty, silently failing store.
	ErrPluginKVPermission = errors.New("plugin kv permission not declared")
	// ErrPluginRequestUnknown marks an inbound method the host does not
	// implement; the transport answers it -32601.
	ErrPluginRequestUnknown = errors.New("plugin request not implemented")
	// ErrPluginRequestParams marks an inbound request whose params could not be
	// decoded. It maps to -32602, so a plugin sees a caller bug rather than a
	// host failure.
	ErrPluginRequestParams = errors.New("plugin request params invalid")
)
