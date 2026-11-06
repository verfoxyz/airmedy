# Plugins

<!-- toc -->
- [Summary](#summary)
- [Everything is a plugin](#everything-is-a-plugin)
- [Core / plugin ownership](#core-plugin-ownership)
- [Files](#files)
- [Manifest](#manifest)
  - [Validation split](#validation-split)
- [Wire protocol](#wire-protocol)
- [Host methods](#host-methods)
- [Online catalogue search](#online-catalogue-search)
- [Lifecycle notification](#lifecycle-notification)
- [Permissions: declared and enforced](#permissions-declared-and-enforced)
  - [Granting the KV right](#granting-the-kv-right)
- [Lifecycle](#lifecycle)
- [Enable state](#enable-state)
- [Routing](#routing)
  - [Dynamic routes (`describe`)](#dynamic-routes-describe)
- [Settings UI](#settings-ui)
- [Managed source lists](#managed-source-lists)
  - [Adding a source](#adding-a-source)
- [Verification](#verification)
<!-- /toc -->

## Summary

Airmedy is a plugin host. Plugins are child processes that speak newline-delimited
JSON-RPC 2.0 over stdio and implement capabilities the app consumes (`stream.resolve`,
`catalog.search` and `lyric.fetch` today; `auth` next). The app implements no **stream
source**: it provides the ports, the lifecycle, the routing, and the plugin's key/value
store (`storage.*`). Lyrics have built-in Kugou/LRCLIB/local `.lrc` providers, and a plugin
that owns a search source may additionally answer `lyric.fetch` for its rows.

The model is two levels, and only two:

```
app (Airmedy)  → ports, lifecycle, routing, UI
plugin         → all functionality; a leaf, with nothing below it
```

**One plugin = one functional unit = one UI row = one switch.** How a plugin
implements its feature (whether it spawns a sandbox, wraps a script, calls a remote
service) is its own business and the app does not model it.

The one exception is content the *user* brings: the lx plugin runs the custom
source scripts the user imports, so its list of sources is app data. The core
stores that list and hands it to the plugin at launch; the plugin still owns every
lx semantic (sandbox, protocol, fallback order). See
[Managed source lists](#managed-source-lists).

## Everything is a plugin

**Every capability is delivered by a plugin, never by the core.** The core may only
grow:

1. **Ports** — a `domain` interface plus the error model for a new capability.
2. **Routing** — which plugin serves a request, and what happens when none can.
3. **Lifecycle** — discovery, enable state, autostart, restart, shutdown.
4. **Shell** — the Wails binding and the UI that lists/toggles/renders plugin output.

Anything else belongs to a plugin. In particular, the remaining roadmap items are
plugins, not features to build in Go:

| Planned capability | Where it lives |
|---|---|
| Streaming playback of a remote track | a plugin declaring `stream.resolve` and the providers it serves |
| Sources (lx custom sources, catalog providers) | one plugin per *kind* of source; user-imported scripts are a managed list under that plugin (`plugins/lx`) |
| Auth and artwork providers | plugins declaring `auth` / `pic.fetch` |
| Lyrics | **built in** (Kugou + LRCLIB + local `.lrc`); a source plugin may add `lyric.fetch` for its own rows |
| Search over an online catalogue | a plugin behind a `catalog.*` capability |

Rule of thumb for review: if a diff adds provider-specific logic — a URL shape, an API
key, a response format, a fallback chain — to `internal/`, it is in the wrong layer. It
belongs in a plugin, and the core only needs a port it can be reached through.

A capability that has no plugin must fail loudly (`ErrStreamUnavailable` with the
provider named), never fall back to a hard-coded provider.

## Core / plugin ownership

The core is a complete local player. With zero plugins installed, scanning, playback,
tags, cover extraction, local and network lyrics (built-in Kugou + LRCLIB + local
`.lrc`), EQ, playlists, favorites and analytics all work. Only fetching a **playback
URL** requires a plugin.

| Capability | Owner | Notes |
|---|---|---|
| Scan, import, playback, EQ, tags, cover, playlists, favorites, analytics | core | |
| Local lyrics (`.lrc` / `.txt` beside the audio) | core | |
| Network lyrics (Kugou / LRCLIB) | core | built-ins remain; a plugin owning the source gets the first attempt through `lyric.fetch` |
| Search over an online catalogue | plugin | `catalog.search` |
| Playback URL | plugin | `stream.resolve`; no built-in provider |
| Download (bytes, validation, tags, naming, import) | core | the plugin supplies only the URL |

Invariants:

1. **Zero plugins boots.** The fx graph builds and starts with no manifest installed;
   only remote playback and download are unavailable.
2. **Local features do not depend on the plugin registry.** The library, player and
   lyrics services hold no plugin dependency.
3. **Download only depends on the port.** `app/download` sees `domain.StreamResolver`
   and knows nothing about lx, a plugin id, or how a URL is obtained.
4. **Failures are readable.** An unresolvable provider names the provider and asks for
   an installed/enabled source. Both the catalog action and the enqueue path refuse it
   before a job is created, so the search action, playlist auto-download, direct call
   and retry cannot leave a doomed job behind.
5. **Plugin-specific logic never enters `internal/`.** A URL shape, API key, response
   format or fallback chain belongs to a plugin; the core grows only ports, routing,
   lifecycle and shell.
6. **No fallback.** A provider with no owner is unplayable; the host never falls back
   to a hard-coded provider. Disabling or removing a plugin makes its providers
   unplayable but keeps already-downloaded local files.

Adding a built-in stream provider to the core is an architecture violation.

## Files

| File | Purpose |
|---|---|
| `internal/domain/plugin.go` | `PluginManifest` (incl. `Providers`), `Capability`, `PluginHost`/`PluginLoader`/`PluginHostFactory`/`PluginSettingsRepository` ports, sentinels, stream error kinds, `describe` / `source.validate` contracts |
| `internal/domain/lx_source.go` | `LxSource` + `LxSourceRepository` port, import limits, `PluginDescribe`, `PluginSourceDescription`, lx sentinels |
| `internal/infra/pluginhost/manifest.go` | manifest parse + per-file validation (providers normalised), discovery |
| `internal/infra/pluginhost/process.go` | child process launch/stop/crash detection, env layering |
| `internal/infra/pluginhost/transport.go` | line framing, request/response correlation, timeouts |
| `internal/infra/pluginhost/host.go` | `Start`/`Call`/`Close`, handshake, restart budget |
| `internal/app/plugin/service.go` | registry: static + dynamic provider tables, conflict rules, enable state, autostart, restart, status |
| `internal/app/lx/service.go` | lx source list: import/validate, enable, remove, reorder, launch environment |
| `internal/app/plugin/module.go` | fx wiring: `Scan` then `StartEnabled` on start, `Close` on stop |
| `internal/app/stream/plugin_resolver.go` | provider lookup → one plugin call, maps error kinds; the only `domain.StreamResolver` |
| `internal/domain/catalog_search.go` | `catalog.search` contract: methods, limits, wire shapes, `CatalogSearcher` / source-registry ports |
| `internal/app/catalog/service.go` | orchestration: fan-out, partial failures, availability |
| `internal/app/catalog/ranking.go` | rank fusion, de-duplication, paging |
| `internal/app/catalog/plugin_adapter.go` | `describe` source table + the per-source plugin call |
| `internal/domain/plugin_kv.go` | `storage.*` contract: methods, limits, wire shapes, `PluginKVRepository` port, request sentinels |
| `internal/app/pluginkv/service.go` | `storage.*` handler: permission gate, limits, JSON-RPC answers |
| `internal/infra/sqlite/plugin_kv_repository.go` | the store (`plugin_kv`, migration 000082) |
| `internal/infra/wails/plugin_service.go` | plugin binding (translation only) |
| `internal/infra/wails/lx_sources_service.go` | lx source binding (translation only) |
| `internal/infra/sqlite/plugin_settings_repository.go` | enable flags |
| `internal/infra/sqlite/lx_source_repository.go` | the imported lx source list (`lx_sources`, migration 000081) |
| `frontend/src/components/settings/PluginsSettings.vue` | plugins section (flat list) |
| `frontend/src/components/settings/LxSourcesSettings.vue` | the lx source sub-page (import, enable, order, remove) |
| `plugins/lx/` | the lx custom-source plugin (`id: lx`) that runs the imported scripts |
| `plugins/wy/` | the aggregate-source plugin (`id: wy`): `catalog.search` + `lyric.fetch`, cover URLs in rows |
| `examples/plugins/` | minimal runnable plugin |

## Manifest

```json
{
  "id": "lx",
  "name": "Lx 自定义源",
  "version": "3.0.0",
  "author": "…",
  "apiVersion": "^1.0",
  "runtime": { "type": "process", "command": "node", "args": ["wrapper.js"] },
  "capabilities": ["stream.resolve"],
  "providers": [],
  "permissions": { "network": ["*"], "kv": true }
}
```

`permissions.kv` is what unlocks `storage.*`; a manifest without it is refused every
storage request (`-32003`). See [Permissions](#permissions-declared-and-enforced).

Every plugin is launched by the app, so `runtime` is always `process` plus a
command; there is no host-defined runtime shape any more.

`providers` is the **routing declaration**: the track provider ids this plugin
serves. Empty (or absent) is legal and means "serves no track source" — a
lyrics-only plugin is a first-class plugin, not a broken one.

`sources` lists the platform ids a plugin serves **without** a stream route (search and
lyrics). It is what lets a search-only plugin receive `lyric.fetch` for the rows it
produced without claiming a playback URL. `description` is an optional one-line text the
settings row shows (for example "聚合多个在线音乐源；当前支持 wy").

A plugin whose routes depend on user content declares **no** providers and reports
them through `describe` instead. `plugins/lx` is the standard case: its manifest
has `"providers": []`, and the registry adopts whatever the running process
reports. The two forms are exclusive per plugin — once a plugin has answered
`describe`, its dynamic list *is* its routing table, including an empty one, so a
disabled source list cannot silently fall back to a stale manifest declaration.

### Validation split

Two layers, because neither can check alone:

**Per file** (`pluginhost.LoadManifest`) — id syntax, semver, `apiVersion` range,
`runtime.type: process` plus a command, and:

| Rule | Failure |
|---|---|
| providers are trimmed, lower-cased, blank-free and deduplicated (`domain.NormalizeProvider`) | — |
| declaring providers requires `stream.resolve` | `ErrPluginManifestInvalid` |

**Registry** (`app/plugin`) — the cross-plugin rule, because only the registry sees
the whole installed set:

| Rule | Failure |
|---|---|
| a provider is declared by exactly one plugin | `ErrPluginProviderConflict` on **both** |

A manifest that fails these is **kept but marked invalid**: it stays in `List()` with
`invalid: true` and a reason, so the settings page explains it instead of the plugin
silently disappearing. The conflict rule is order-independent — discovery order never
picks a winner, because the user could not predict it — and the reason is derived, so
a rescan recomputes it rather than latching.

## Wire protocol

Newline-delimited JSON-RPC 2.0 over the child's stdio. One line is capped at
`maxMessageSize` (8 MiB, `transport.go`): a longer line ends the session rather than
being truncated, so no method may carry bulk bytes. The plugin's stderr goes to the
host logger at debug level, prefixed with the plugin id.

| Bound | Value |
|---|---|
| handshake (`initialize`) | 10 s, then the process is killed |
| one `Call` | 15 s default; a caller context may shorten it |
| close grace | 3 s, then SIGKILL |
| restarts after a crash | 3, then the plugin is marked unavailable |

**Handshake.** The host opens every plugin with `initialize`
(`{hostVersion, apiVersion, locale}` → `{apiVersion, capabilities}`) and closes it with
`shutdown` before the SIGTERM fallback. A version the manifest's range does not cover
is refused — no "run anyway". An empty `capabilities` answer falls back to the manifest
so the UI can still say what the plugin was installed for. There is no `ping`;
liveness is the process plus the restart budget.

**Error codes** a plugin may return, and what the host does with them:

| Code | Meaning | Retryable |
|---|---|---|
| `-32000` | plugin process crashed | yes — the host relaunches |
| `-32001` | handshake timed out / never completed | no |
| `-32002` | API version incompatible | no |
| `-32601` | method not found | no |

**The host answers exactly four methods**, all under `storage.` (see
[Host methods](#host-methods)). Every other plugin→host request is still `-32601`
(`transport.answerInbound`). There is no `http.request` — see
[Network](#permissions-declared-and-enforced) — and no plugin-to-plugin call.

## Host methods

These are the only requests the host serves. They are the plugin's persistence:
a child process has no store of its own, and the alternative (a plugin writing into
the app database) would hand third-party code a schema it must not see.

| Method | Params | Result |
|---|---|---|
| `storage.get` | `{key}` | `{key, value, found}` — a missing key is `found: false`, not an error |
| `storage.set` | `{key, value}` | `{}` — upsert |
| `storage.delete` | `{key}` | `{deleted}` — deleting a missing key is not an error |
| `storage.keys` | *(none)* | `{keys}` — sorted, never `null` |

- **Isolation is by plugin id.** It comes from the manifest and is the same value
  every other registry decision uses; one plugin cannot read another's keys.
- **`value` is opaque.** The plugin's own JSON travels as a string and the host
  never parses it, so a plugin can change its value shape without a host version
  bump.
- **Limits:** 64 KiB per value, 128-byte keys, 256 keys per plugin. The key cap blocks
  only a *new* key — a plugin at the limit can still update what it already owns.
- **The default is deny.** A manifest without `permissions.kv` gets `-32003` on every
  `storage.*` call. See [Permissions](#permissions-declared-and-enforced).

A handler that is not bound keeps the pre-KV answer (`-32601`), so an older host
behaves exactly as it did before. The handler runs in the reader goroutine: a concurrent
`storage.get` inside an open call works (the tests drive this), and a panic in it is
answered `-32603` instead of ending the session.

## Online catalogue search

A plugin that declares **`catalog.search`** can be asked for keyword results from one
source. It is a *different* capability from `stream.resolve`: searching a catalogue and
producing a playable URL are separate endpoints with separate failure modes, and a plugin
may do one without the other.

**Method.** `catalog.search` — params `{source, query, limit, offset, include_cover}` →
result `{rows, hasMore}`. A row is `{externalId, title, artists[], album, duration, vip,
cover_url, cover_url_large}`. `cover_url` is the list thumbnail; `cover_url_large` is the
500×500 (or original) rendition used for the file's embedded artwork and the library cover.
Both are optional; `include_cover` is the plugin's declared "show covers in results" switch,
sent by the host so a plugin can omit the URLs. The host caches the large URL it saw and,
when a save has none, re-asks the source once with covers forced on, so hiding thumbnails
does not strip artwork from the downloaded file. The host asks for **one source per call**:
it resolves "全部" into one call per source, so no plugin ever fans out internally.

**The source list comes from `describe`.** A search plugin reports
`searchSources: [{id, name}]` on its existing `describe` answer — the same mechanism the
lx plugin uses for routes. Adding a source is therefore a plugin change with **no new
protocol method** and no plugin-to-plugin calls (see `docs/plugin-api.md` §6.1).

**One plugin per source.** `plugins/wy` is the first implementation and the reference
shape: one process, one source, no credentials. It declares `catalog.search` and no
`stream.resolve` — search and resolve are separate capabilities — reports
`searchSources: [{id: "wy"}]`, and maps HTTP/body failures onto the three kinds above
(`rate_limited` for 429, `auth` for 401/403 and the platform's login-required codes,
`unavailable` for timeouts, 5xx, unreadable JSON and everything else). A caller mistake
(blank query, unknown source) is `-32602` instead: retrying cannot grow correct params.

Adding `kg` / `kw` / `mg` / `tx` is one plugin each, not a branch in an existing one: the
platforms share neither endpoints nor signing nor rate limits, and the host and the search
screen must not change when one is added.

**Host responsibilities** (`internal/app/catalog`):

| Concern | Rule |
|---|---|
| fan-out | one concurrent call per selected source, each with its own 10 s budget |
| partial failure | a failed source is reported alongside the rows; it never blanks the page |
| ranking | reciprocal-rank fusion `Σ 1/(rank+3)` over the *source's row order*, plus a title/artist hit bonus. A source's own "score" is never used — the platforms' scales are not comparable |
| identity | `(source, externalId)`. The same track from two sources stays two rows, each naming its own source |
| paging | 50/page over the *merged* list; `hasMore` is derived from the slice, not from a full page |
| downloadability | a row is enabled only when a resolver claims its provider; otherwise `no_resolver` disables it with a reason ("可搜 ≠ 可下") |

**Errors.** A plugin reports `data.kind` of `rate_limited` / `unavailable` / `auth`; the
message is never parsed. A plugin that declares the capability without implementing the
method is reported as such rather than retried.

**Limits.** 50 rows per page, at most 32 reported sources, 200 characters of query. The
host caps the row count it accepts regardless of what a plugin returns, because one
answer travels as a single line (8 MiB).

## Lyrics from a source plugin

`lyric.fetch` lets the plugin that searched a row also supply its lyric. Params are
`{provider, external_id, title, artists[], album, duration}`; the result is
`{lyric, tlyric, source, synced}`. The host routes to the running plugin whose manifest
`sources` contains the track's provider and that declares `capability: lyric.fetch`.

- **The plugin must return a timeline.** The plugin rejects a body without `[mm:ss]`; the
  host validates it again before storing, because a plain text body cannot scroll.
- **`tlyric` is stored separately.** The host persists `tlyric` as
  `translation_content` and never concatenates it into the original. It is not required
  to carry a timeline: a timed translation aligns by timestamp, an untimed one by line
  index. An empty `tlyric` leaves the translation columns empty.
- **Fallback is mandatory.** A plugin failure (`unavailable` / `rate_limited` / `auth`)
  or a non-timeline answer falls through to the built-in LRCLIB/Kugou providers, so a
  platform outage never removes lyrics entirely.
- **No title/artist second lookup for plugin rows.** The plugin already knows the platform
  id, which is exactly the case a title+artist search misses (Japanese titles, covers).
- `synced` is the plugin's claim; the host still requires the timeline tags above.

## Lifecycle notification

Before the shutdown request, the host sends **`lifecycle.unload`** as a notification
(no id, so nothing waits for an answer) with `{reason}`:

| reason | When |
|---|---|
| `disabled` | the plugin was switched off, or stopped without a stated reason |
| `reload` | the host restarted it on purpose (a source list change) |
| `shutdown` | the application is exiting |

It is a **hint, not a synchronisation point**: the process is reclaimed by the same
grace → EOF → terminate → kill ladder either way. A plugin that ignores it is unaffected;
one that honours it can dispose sandboxes and cancel in-flight work
(`plugins/lx/wrapper.js` does exactly that) instead of waiting for SIGTERM.

**API compatibility is a published contract.** A `1.x` host only ever adds methods and
optional fields — it never removes one and never changes a semantic, so a plugin
written for an older 1.x minor keeps working while one claiming a newer minor is not
loaded. Nothing outside this port is promised (internal packages, DB schema, UI
components).

**Unknown values are recorded, not rejected.** Unrecognised `capabilities`, provider
keys and quality strings pass through. Third-party lx scripts register platform keys
outside lx's own whitelist, and strict validation would drop half of them with nothing
to tell the user why (see `plugins/lx/README.md`).

## Permissions: declared and enforced

The manifest's `permissions` block now has two halves, and they behave differently.

| Key | Enforcement | Where |
|---|---|---|
| `permissions.network` | **declared only** | parsed and trimmed in `manifest.go`; nothing else |
| `permissions.kv` | **enforced** | every `storage.*` request is refused `-32003` unless it is `true` (`app/pluginkv`) |

`network` is a declaration because plugins are ordinary child processes that fetch
directly (`plugins/lx/src/request.js`): the host sees no request and can neither allow
nor deny one. The settings row shows a globe badge and must keep saying "may access",
never "is allowed to". `kv`, by contrast, is a capability the host itself serves, so
there is a real gate to apply — and it fails closed.

Do not re-propose a host-mediated `http.request` as the network control point: routing
a response back through the stdio line protocol means a body near the 8 MiB cap kills
the session outright, making the proxy strictly weaker than the direct fetch plugins
already have. A real control point needs a separate channel.

### Granting the KV right

`permissions.kv` is checked against the **live registry entry**, not a copy captured at
launch, so a rescan after editing the manifest takes effect without an app restart. An
unknown plugin id has no entry and therefore no permission. The permission table is
attached to the handler after construction (`AttachPermission`) because the registry
consumes the host factory that carries the handler; a constructor argument would close
an fx cycle. `main.go` wires it in one `fx.Invoke`.

## Lifecycle

```
OnStart:  Scan() ──▶ register all ──▶ resolve provider claims ──▶ load enabled flags ──▶ StartEnabled()
OnStop:   Close() ──▶ StopAll()

SetEnabled(id, true)  ──▶ persist ──▶ Start(id)
SetEnabled(id, false) ──▶ persist ──▶ Stop(id)
```

`StartEnabled` launches every enabled, valid plugin, subject to `MaxRunning` (8) and
`MaxConcurrentCalls` (16). A failure is recorded on the entry (`status: failed` plus
the reason) and returned joined; it never aborts the app or the other plugins.

There is no cascade and disabling a plugin affects only itself. The one injected
value is the per-plugin launch environment (`plugin.Config.LaunchEnv`, wired in
`main.go`): the registry asks the app layer for extra `KEY=VALUE` pairs at every
start and merges them over the manifest's `runtime.env`. It is how the lx source
list reaches the plugin — a change in the list is applied by **restarting** the
plugin (`Service.Restart`), not by a hot-reload protocol, because a relaunch costs
~200ms and the user caused the change themselves by saving.

## Enable state

Persisted in `plugin_settings` (migration 000080). **A missing row means disabled** —
enabling a plugin launches third-party code, so the safe default is off. Rows are keyed
by plugin id and nothing else; there is exactly one row per plugin and one switch.

## Routing

A provider is a manifest declaration, not a plugin id, so routing is a **lookup**:

```
provider == ""              → ErrStreamUnavailable (the track has no provider)
provider in the table       → that plugin's stream.resolve
not in the table            → ErrStreamUnavailable (names the provider)
```

There is **no built-in resolver**. The host used to resolve the public NetEase endpoint
itself; that path is retired, so a provider no enabled plugin declares is unplayable even
if another plugin can search it. The table is built from the manifests the registry holds
(`Service.HostForProvider`). Consequences:

- **exactly one plugin is called** per resolve — no plugin is ever asked "is this
  yours?", so there is no decline signal in the protocol and no `unknown_source`
  kind;
- a provider two plugins declare has no owner: both are invalid, and a resolve for it
  is a routing miss;
- a plugin that fails owns the failure; the resolver does not fall through to another
  plugin, because that would silently swap the user's chosen source.

The plugin still reports a **structured failure kind** in `error.data.kind`
(`unavailable` / `vip` / `auth` / `rate_limited`), and the resolver maps it to the
retry policy the download service uses. Wording is never parsed.

### Dynamic routes (`describe`)

A plugin may implement `describe` to report its routes and their sub-sources:

```json
// request: {"jsonrpc":"2.0","id":9,"method":"describe"}
{ "providers": ["wy","kg"],
  "sources": [{ "id": "…", "name": "ikun", "enabled": true, "providers": ["wy"] },
              { "id": "…", "name": "broken", "enabled": true, "providers": [], "error": "…" }],
  "searchSources": [{ "id": "wy", "name": "网易云音乐" }],
  "settings": [{ "key": "cover_in_results", "type": "switch", "default": true,
                 "label": { "zh": "搜索结果显示封面", "en": "Show covers in results" } }] }
```

- The registry calls it **after every successful start**, so a freshly launched
  dynamic plugin is routable immediately, and `Service.Restart` re-asks afterwards.
- The answer replaces the manifest declaration, including when it is empty. A
  plugin that never answers keeps its static list: a `-32601` is "not supported",
  not a failure.
- `providers` is normalised like a manifest (trimmed, lower-cased, de-duplicated,
  blanks dropped), so the routing table and the UI agree on case.
- A source that failed to load must be **absent** from `providers` — routing to it
  would guarantee a failure the retry policy cannot fix.

The plugin is authoritative about what it actually loaded; the database row says
what the user wants. The settings page joins the two and shows a source whose load
failed with its reason.

## Settings UI

Settings → Plugins renders the flat `ListPlugins()` result: one row per plugin with
name, version, author, capability tags, provider tags, a status dot and **its own
switch**. There is no grouping, no indentation and no "host disabled" state — those
existed only for sub-plugins, which no longer exist.

The row for the `lx` plugin additionally links to a **sub-page** (`plugins-lx` in
`SettingsView`'s `activeCategory`, a sibling category rather than a nested route):
the source list, its import button, per-row enable switch, up/down order handles and
delete confirmation. The row's plugin id is the only thing the page keys on; the
backend owns everything else. This is not the old host model coming back — it is
app data (the user's scripts), not a plugin hierarchy.

An invalid plugin (conflict, or providers without `stream.resolve`) shows the
`Unavailable` status with its switch disabled and the reason behind "Show error".

A plugin that declares `permissions.network` also shows a **globe badge** next to its
status, with the declared hosts in the tooltip, and stays visible while the plugin is
disabled so the user can see what it reaches before enabling it. The badge is a
**declaration, not enforcement**: `internal/domain.PluginManifest.Permissions` is only
parsed and trimmed — the host runs plugins as ordinary child processes and does not
sandbox their traffic, so the wording must not promise a sandbox. `manifest.go` also
trims the list, so the badge shows what the file says; `"*"` renders as "any address"
rather than as one more named host.

A plugin that declares `permissions.kv` shows a **storage badge** (`HardDrive`) in the
same place. Unlike the globe this one describes an **enforced** right, so its wording
states the capability ("may store data on this device") rather than a promise. Both
badges dim with opacity while the plugin is disabled and stay legible, because
"what will this get access to?" is the question being decided at that moment.

A plugin may also declare **switches** through `describe.settings` (today only
`type: "switch"`). The settings row renders them under the plugin's own toggle; the
value is stored in the plugin's KV under the host-owned `host.setting.` prefix and is
passed to the plugin on the call it affects (`include_cover` for `catalog.search`).
A plugin that declares no settings keeps the documented defaults.

Empty state names `<DataDir>/plugins/` and offers a refresh button that calls
`RescanPlugins` (re-scan + reload flags) without an app restart.

## Managed source lists

Some plugins wrap content the user brings: the lx plugin runs arbitrary
`.js` custom sources. The user picks the files, so the file set is app data, and the
plugin — a child process with no persistence — cannot own it.

The split is strict:

| Concern | Owner |
|---|---|
| File storage, lifetime, list CRUD, order, enable flags | **core** (`internal/app/lx`, `lx_sources`, `<DataDir>/plugins/lx/sources/`) |
| Sandbox execution, lx protocol, URL parsing, fallback order | **plugin** (`plugins/lx`) |
| Route reporting | **plugin** (`describe`) |

Flow:

```
import:   pick .js → copy to sources/<uuid>.js → ask the plugin `source.validate`
          → reject on syntax error / no registered platform → store row
change:   write the row, then restart the plugin (it reads the list from env)
launch:   AIRMEDY_LX_SOURCES=[{"id","name","path","enabled","position"},…]
```

`source.validate` is a plugin method, not core logic: only the real lx runtime can
decide whether a file registers a platform, and the core implements no lx semantics
(see the rule of thumb above). Importing with the plugin switched off starts it for
the duration of the check and stops it again.

Validation limits: `.js` only, ≤ 2 MiB, and at least one registered platform. The
list is capped (20 by default); every entry is a sandbox in one process.

### Adding a source

There are now two ways to add one, and they are different things:

| Goal | Do this |
|---|---|
| Add a custom source the user has as a `.js` file | Settings → Plugins → Lx 自定义源 → **Add source** (no plugin code) |
| Add a new *kind* of source (a different protocol, a remote provider) | copy `plugins/lx/`-style structure: own `id`, own `providers` declaration |

## Verification

| Concern | Test |
|---|---|
| manifest `providers` normalisation / capability requirement | `internal/infra/pluginhost/manifest_test.go` |
| provider conflict marks both sides, reason is derived | `internal/app/plugin/lifecycle_test.go` |
| routing hit calls only the owner; miss names the provider | `internal/app/stream/provider_routing_test.go` |
| plugin without providers is valid and never routed | `internal/app/plugin/lifecycle_test.go`, `provider_routing_test.go` |
| describe replaces static routes, empty answer wins, -32601 keeps static | `internal/app/plugin/dynamic_test.go` |
| restart re-reads routes; launch env injected per start | `internal/app/plugin/dynamic_test.go` |
| import copies + validates + rejects; order/env format | `internal/app/lx/service_test.go`, `internal/infra/sqlite/lx_source_repository_test.go` |
| lx plugin end to end: describe, fallback, all-fail, validate | `internal/infra/pluginhost/lx_plugin_test.go`, `plugins/lx/test/wrapper.test.js` |
| lx source binding join + plugin-off explanation | `internal/infra/wails/lx_sources_service_test.go` |
| lx source page (import, toggle, confirm delete, reorder) | `frontend/src/components/settings/LxSourcesSettings.spec.ts` |
| enable persistence, autostart isolation | `lifecycle_test.go`, `sqlite/plugin_settings_repository_test.go` |
| example plugin end to end | `internal/infra/pluginhost/example_plugins_test.go` |
| binding DTO shape (flat, no host fields) | `internal/infra/wails/plugin_service_test.go` |
| settings page (flat rows, providers, no host_disabled) | `frontend/src/components/settings/PluginsSettings.spec.ts` |
| network declaration reaches the row (badge, `*`, hosts) | `plugin_service_test.go`, `PluginsSettings.spec.ts` |
| `permissions.kv` reaches the row (enforced badge) | `plugin_service_test.go`, `PluginsSettings.spec.ts` |
| catalog search: fusion, dedup, paging, availability, partial failure | `internal/app/catalog/service_test.go` |
| catalog search: cover field, cover cache, `include_cover` forwarded from the plugin switch | `internal/app/catalog/cover_cache_test.go`, `plugin_adapter_test.go` |
| plugin settings: switch value round-trips in plugin KV with the host prefix | `internal/app/plugin/settings_test.go` |
| lyrics: plugin route first, built-in fallback on failure/absence | `internal/app/lyrics/lyrics_service_test.go` |
| lyric.fetch routing by manifest `sources`, timeline validation | `internal/app/plugin/lyrics_test.go` |
| catalog search: source table from describe, routing, kind mapping, row cap | `internal/app/catalog/plugin_adapter_test.go` |
| catalog search: the plugin resolver is the availability oracle; registry is the source table | `internal/app/stream/catalog_wiring_test.go` |
| no built-in resolver; an undeclared provider is a readable permanent failure | `internal/app/stream/plugin_resolver_test.go` |
| built-in lyrics providers exist with no plugin module | `main_test.go` |
| `storage.*` served only with the permission; -32003 without it | `internal/app/pluginkv/service_test.go`, `internal/infra/pluginhost/kv_permission_test.go` |
| key/value validation, key cap blocks new keys only, isolation | `internal/app/pluginkv/service_test.go` |
| KV upsert/delete/sorted keys, per-plugin isolation | `internal/infra/sqlite/plugin_kv_repository_test.go` |
| inbound requests answered mid-call; unknown method keeps -32601 | `internal/infra/pluginhost/inbound_test.go` |
| no handler bound keeps the pre-KV answer | `internal/infra/pluginhost/inbound_test.go` |
| a handler panic does not kill the session | `internal/infra/pluginhost/inbound_test.go` |
| `lifecycle.unload` reason reaches the plugin (disabled/reload/shutdown) | `internal/infra/pluginhost/unload_test.go`, `internal/app/plugin/service_test.go` |
| lx plugin honours `lifecycle.unload` and disposes its sandboxes | `plugins/lx/test/wrapper.test.js` |
| locale parity | `frontend/src/locales/plugin-keys.spec.ts` |
| lx plugin protocol, describe and source validation | `plugins/lx/test/wrapper.test.js` |