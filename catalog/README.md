# Catalog

Contract reference for **Airmedy**, one directory per feature. Each directory holds a
single `README.md` that describes the boundary, the data flow, the invariants that
constrain future change, and the files that own them.

This file is the index. Read only the entry for the area you are touching, and only
when its documented boundary, lifecycle, data flow or external contract could be
affected (AGENTS.md §8).

Design plans and verified machine constraints live in [`../docs/`](../docs/), not here.

---

## Areas

| Area | Owns | Invariant worth knowing before you touch it |
|---|---|---|
| [`architecture/`](architecture/README.md) | Layering (`infra → app → domain`), FX wiring, Wails IPC | `app` never imports `infra`; adapters are bound to ports in `main.go`. |
| [`database/`](database/README.md) | SQLite schema, golang-migrate migrations, repositories | Every schema change ships a down path; migration tests pin a version. |
| [`library/`](library/README.md) | Watched folders, scanning, import, rescan | Remote rows survive every rescan and every folder removal. |
| [`metadata/`](metadata/README.md) | Tag reading, normalization, tag writing | Audio files are read-only unless the user triggers a metadata save. |
| [`artwork/`](artwork/README.md) | Artwork cache, resized variants, palettes, cleanup | Artwork is content-addressed by key; cleanup is safe to re-run. |
| [`analysis/`](analysis/README.md) | Background decode + loudness/dynamics pipeline | One analysis per track; results are keyed by track id. |
| [`normalization/`](normalization/README.md) | Per-track pre-amp gain | Gain is applied at playback and is never written into files. |
| [`lyrics/`](lyrics/README.md) | Local lyric files, network providers, stored lyrics | Local `.lrc` wins when present; lyrics live in the database, not in the audio file. |
| [`search/`](search/README.md) | Bleve index and query | One global index; document ids are `{type}:{id}`. |
| [`playlists/`](playlists/README.md) | Ordered membership, smart playlists, favorites | Favorites is a virtual playlist derived from `tracks.is_favorite`, not membership rows. |
| [`player/`](player/README.md) | Playback, queue, restore, OS media integration | Queue state survives restarts, including rows that are not downloaded yet. |
| [`equalizer/`](equalizer/README.md) | 10-band EQ, profiles, presets | EQ is applied in the audio adapter, never baked into files. |
| [`plugins/`](plugins/README.md) | Plugin host, lifecycle, routing, permissions, host methods | Provider-specific logic never enters `internal/`; a provider has exactly one owning plugin. |
| [`download/`](download/README.md) | Remote row → local file pipeline | One active job per track; the stream URL is re-resolved before every attempt. |
| [`settings/`](settings/README.md) | Application settings and the settings UI | Settings are one row; theme tokens are resolved from them at runtime. |
| [`ui/`](ui/README.md) | Vue SPA, design system, stores, routing | Every colour resolves from a theme token; icons are Lucide only. |
| [`frontend-monorepo/`](frontend-monorepo/README.md) | pnpm workspaces, `@airmedy/ui`, `@airmedy/utils` | `packages/ui` is stateless: no stores, bindings or router. |
| [`remote/`](remote/README.md) | LAN HTTP + WebSocket control server and SPA | Same use cases as the desktop UI; no second backend. |
| [`pairing/`](pairing/README.md) | Ed25519 pairing between desktop and mobile | A device is trusted by an explicit cryptographic exchange, never by being on the same network. |
| [`sync/`](sync/README.md) | Desktop → mobile library pull | Runs only after pairing; the desktop stays the source of truth. |

---

## Where a fact lives

A fact has exactly one home, chosen by who has to act on it. Everywhere else it is a
link, never a restatement.

| Kind of fact | Home | Example |
|---|---|---|
| A contract that constrains code | `catalog/<area>/` | the stream URL comes from a plugin |
| A fact about this machine or toolchain | [`docs/windows-build.md`](../docs/windows-build.md) | temp dirs, broken `wails3 task` targets |
| A design or decision not implemented yet | `docs/*-plan.md`, [`docs/plugin-api.md`](../docs/plugin-api.md) | streaming playback, cache model |
| A rule a contributor must follow | [`AGENTS.md`](../AGENTS.md) | generated bindings are never hand-edited |
| User-facing explanation | [`README.md`](../README.md) | install, screenshots |

If two places disagree, the one in this table wins and the other is a bug.

## Conventions

- **English, operational, ownership first.** Record the contracts that constrain
  future change; point at the source of truth instead of copying implementation.
- **Delete stale detail instead of appending a correction below it.**
- Keep each file readable in one pass: boundary and invariants near the top, details
  below.
- A `Files` table names the code that owns each concern; that table is the fastest
  way in.
- Cross-reference other areas with relative links, never with duplicated prose.
- A file over 400 lines carries a `<!-- toc -->` block (generated by
  `node scripts/check-docs.mjs --fix`); a file over 800 lines is split by concern into
  sibling files in the same directory.

## When to update

Change a file here in the same task only when it would otherwise state something
false or omit what is needed to use the contract safely: public setup and commands,
DB schema and migrations, Wails APIs and events, cross-layer ports, lifecycle
invariants, persisted data flow, user-visible behaviour, navigation ownership,
algorithm semantics.

Never for private refactors, renamed internals, localized fixes, tests,
CI/build/lint changes, dependency bumps or formatting (AGENTS.md §8).
