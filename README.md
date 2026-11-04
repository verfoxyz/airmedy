# Airmedy — plugins branch

> **Unofficial fork.** This repository exists to develop the **plugin version** of Airmedy.
> For features, screenshots, installers and general documentation, see the upstream project:
>
> **→ [github.com/misa198/airmedy](https://github.com/misa198/airmedy)**

---

## What this branch is

- Airmedy's desktop app plus a **plugin host**: plugins are child processes that speak
  newline-delimited JSON-RPC over stdio and implement capabilities the app consumes.
- **Plugins are not distributed here.** A user installs them into the app's data directory
  (`<data dir>/plugins/<id>/`), which is why nothing under `plugins/` is tracked in this
  repository.
- Everything else — the music player, library, playback, UI — is upstream work, documented,
  released and supported upstream.

## ⚠ Before you install

- **It replaces the official Airmedy.** Same app name and bundle id, so the two cannot be
  installed side by side.
- **It shares the official data directory** (`%LOCALAPPDATA%\airmedy` on Windows). This build
  migrates the database forward, so going back to the official build afterwards may fail —
  back up that directory first.
- **The updater still points at the official repository.** "Check for updates" will offer the
  official build, and installing it silently undoes this branch.

## Upstream

| | |
|---|---|
| Source | <https://github.com/misa198/airmedy> |
| Releases | <https://github.com/misa198/airmedy/releases> |
| Site / downloads | <https://airmedy.pages.dev/> |

This branch tracks upstream `master`; diffing the two shows exactly what the plugin work adds.

## Building this branch

The build is unchanged from upstream. Local machine constraints (Windows, cgo, temp dirs) are
recorded in [`docs/windows-build.md`](docs/windows-build.md); the plugin contract lives in
[`catalog/plugins/`](catalog/plugins/README.md).

## License

GPL-3.0 © [misa198](https://github.com/misa198). Third-party licenses in [NOTICES](NOTICES).

This fork is distributed under the same license as upstream.
