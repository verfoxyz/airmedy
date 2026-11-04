# Airmedy Project Mandates

Operating rules only. Facts live in `catalog/` (per-feature contracts, English) and
`docs/` (design plans, machine constraints). This file duplicates neither.

## Before You Act

- `agents/INTERACTION.md` — Inquiry vs Directive. An inquiry gets research and a
  proposal, never a file edit.
- `mobile/AGENTS.md` — read before touching `mobile/`; it overrides the desktop UI,
  testing and platform rules for that subtree.
- `agents/WAILS_V3.md` — v3 is alpha; when a v2 idiom fails, find the v3 equivalent
  instead of assuming the same behaviour.

## Verified Environment Facts

Recorded once in [`docs/windows-build.md`](docs/windows-build.md); that file is the only
source for them. Two of its facts change how every task is run, so they are repeated here
as commands rather than explanations — the reasons live in the document:

```bash
export PATH="/d/PFilesD/MSYS/mingw64/bin:$(go env GOPATH)/bin:$PATH"
export CGO_ENABLED=1 GOTMPDIR="$PWD/.gotmp" TMP="$PWD/.gotmp" TEMP="$PWD/.gotmp"
```

Launch built binaries from `%TEMP%`, never from the workspace, and do not call
`wails3 task windows:build` / `wails3 task verify` from Git Bash — the documented
equivalents are in the same file. If a build or test fails in a way that smells like the
environment, read that file before debugging code.

## Tech Stack

Wails v3 (Go + Vue 3) · pnpm workspaces + Turbo · Uber FX · Pinia · vue-i18n · Radix Vue
+ Tailwind v4 (`@airmedy/ui`) · SQLite (sqlx + golang-migrate) · Bleve v2 · fsnotify ·
SFBAudioEngine (macOS, cgo) / miniaudio (Win + Linux) · TagLib + FFmpeg ·
`log/slog` + lumberjack.

## 1. Performance

- Track lists and grids use the virtualised `TrackTable` (`vue-virtual-sortable`); row
  heights live in `catalog/ui/README.md`.
- Heavy work (metadata, scanning, indexing) runs in goroutines honouring
  `context.Context` cancellation.
- SQLite: transactions for bulk writes; keep the indexes honest.

## 2. UI Design System

Numbers — layout, type scale, glass recipes — live in `catalog/ui/README.md`. The rules:

- Icons: **Lucide Vue only.**
- **Every colour resolves from a theme token.** No hex, `rgb`/`rgba`, named colours or
  Tailwind palette utilities (`white`, `red-*`, `zinc-*`, `slate-*`) in components. The
  only exceptions are token declarations in global theme CSS and runtime artwork values.
- No white- or black-based opacity overlays for interaction states; derive them from the
  active theme tokens.
- Inactive → active is an opacity step of a theme token, not a colour switch.
- Row hover: `hover:bg-foreground/[0.04]`. Never `hover:bg-accent/50`.
- Secondary text: `text-dim` / `text-subdued` / `text-dimmer`. `text-muted-foreground`
  is banned on dark translucent backgrounds and survives in ~5 legacy files — do not
  add to that set.
- Glass is for **floating surfaces only** (menus, popovers, drawers, modals, home
  cards). Persistent chrome is opaque `bg-background` with `border-foreground/[0.06]`
  hairlines.
- Tailwind v4: new design tokens go in `@theme` in global CSS. Colour utilities are
  aliased to CSS variables in `tailwind.config.js` (loaded via `@config`) — extend the
  right one, never inline a literal value.
- Check light, dark **and** black after any colour change.
- Sliders → `Slider`, text inputs → `Input`, both from `@airmedy/ui`. Never a raw
  `<input>`.
- **pnpm only.**

## 2.1 Internationalization

- 12 locales — `de en es fr it ja ko pt ru th vi zh` — in `frontend/src/locales/`.
- No hardcoded strings. `$t()` in templates, `t()` from `useI18n()` in scripts. Never
  `t('key', 'Fallback')`; the key must exist.
- Add a key to **all 12 files in the same change**.

## 2.5 Component Placement

- `packages/ui/src/` — stateless primitives, no stores/bindings/router.
  `frontend/src/components/` — feature components. `frontend/src/views/` — orchestration
  and page-level fetching.
- Extract a composable or component on the third repetition; prefer `<slot />` over prop
  sprawl; split past ~300 lines.
- `<script setup lang="ts">` with typed `defineProps<{}>()` / `defineEmits<{}>()`.
- Every non-trivial feature component ships a `.spec.ts` (Vitest + `@vue/test-utils`).

## 2.6 Frontend Memory Management

`Events.On()` returns an off-function — **always keep it**. A Pinia store that calls it
collects off-fns in an array, exposes `dispose()`, and guards `init()` against a double
call with an `_initialized` flag.

Everything below must be released in `onUnmounted` (or the store's `dispose()`):

| Resource | Cleanup |
|---|---|
| `Events.On()` | call the returned off-fn |
| `setInterval` / `setTimeout` | `clearInterval` / `clearTimeout` |
| `requestAnimationFrame` loop | `cancelAnimationFrame` |
| `ResizeObserver` / `IntersectionObserver` / `MutationObserver` | `.disconnect()` |
| `addEventListener` on `window`/`document`/`MediaQueryList` | `removeEventListener` with the **same function reference** (store it) |
| WebGL / Canvas context | `loseContext()` |
| `URL.createObjectURL` | `URL.revokeObjectURL` |

## 3. Go Architecture

Strict dependency direction: `infra → app → domain`. Full map in
`catalog/architecture/README.md`.

- `domain` — entities and port interfaces; imports neither `app` nor `infra`.
- `app` — use cases; never imports `infra`. Adapters are bound onto ports in `main.go`
  via `fx.Module`s.
- `infra` — adapters. `wails/` is translation only: no logic in bindings.
- The entry point is the root `main.go`; there is no `cmd/`.
- Errors: `fmt.Errorf("context: %w", err)`. Domain errors are defined in `domain` and
  turned into user messages in `infra/wails`.
- Logging is `*slog.Logger` (FX-injected) or package-level `slog.*` after startup —
  never `log.Print*` / `fmt.Print*` in production.
- **Provider-specific logic** — a URL shape, an API key, a response format, a fallback
  chain — inside `internal/` is in the wrong layer. It belongs in a plugin.

## 4. Git & Commits

Conventional Commits: `type(scope): description`. Types `feat fix chore docs style
refactor perf test`; scopes `core app infra domain ui meta`. Hooks: `task setup:hooks`
(enforces `commit-msg`).

## 5. Data Integrity

- Schema changes go through golang-migrate; no destructive migration without a down path.
- Music files are **read-only** unless the user explicitly triggers Save Metadata.
- Propagate meaningful errors from Go to the frontend through Wails.

## 6. Testing

- Every bug fix and feature ships with a Go test or a Vue component test.
- Run `task verify` (or the documented equivalent) before declaring completion.
- Done means tests pass **and** the affected UI was actually verified. If you could not
  check it visually, say so explicitly instead of implying a check happened.

## 7. Platform Integration

Now Playing / media keys / SMTC / MPRIS stay in sync with playback state, and "close to
tray" keeps playing. Per-platform detail: `catalog/player/README.md`.

## 8. Catalog Maintenance

`catalog/` is a contract reference, not a changelog. Read only the entry for the area
you are touching, and only when its documented boundary, lifecycle, data flow or
external contract could be affected. Isolated CI, build, lint, test, formatting or
dependency changes need no catalog read.

| Area | Catalog |
|---|---|
| Library scan, folder watch, import | `library/` |
| Playback, queue, player state | `player/` |
| Tags, normalization, metadata write | `metadata/` |
| Bleve index and query | `search/` |
| Artwork cache, palette, asset handler | `artwork/` |
| EQ profiles and presets | `equalizer/` |
| Background analysis pipeline | `analysis/` |
| Volume normalization | `normalization/` |
| Lyrics fetch, parse, display | `lyrics/` |
| Playlists, ordering, artwork | `playlists/` |
| Download queue, remote tracks, merge | `download/` |
| Settings, theme, language | `settings/` |
| SQLite schema, migrations, repositories | `database/` |
| Vue components, stores, routing | `ui/` |
| pnpm monorepo, `@airmedy/ui` | `frontend-monorepo/` |
| Architecture, DI, Wails IPC | `architecture/` |
| Remote server, WS protocol, auth | `remote/` |
| Plugin manifests, lifecycle, routing | `plugins/` |

**Update threshold.** Change a catalog in the same task only when it would otherwise
state something false or omit what is needed to use the contract safely: public setup
and commands, DB schema and migrations, Wails APIs and events, cross-layer ports,
lifecycle invariants, persisted data flow, user-visible behaviour, navigation ownership,
algorithm semantics. Never for private refactors, renamed internals, localized fixes,
tests, CI/build/lint changes, dependency bumps or formatting.

**Writing.** English, operational, ownership first. Record the contracts that constrain
future change; point at the source of truth rather than copying implementation detail.
Delete stale detail instead of appending a correction below it.

## File Map

- `AGENTS.md` — the rules (this file).
- [`docs/plugin-api.md`](docs/plugin-api.md) — plugin interfaces not yet implemented,
  plus the decisions not to revisit.
- [`docs/windows-build.md`](docs/windows-build.md) — verified machine constraints and
  crash root causes.
- `catalog/` — per-feature contracts, English (§8).
- `agents/` — interaction protocol and Wails v3 notes.
- `README.md` — this branch's scope; the project itself is documented upstream.