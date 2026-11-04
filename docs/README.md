# docs/

Long-form notes that do not belong in `catalog/` (which holds per-feature contracts)
or in a code comment.

| File | Use it when |
|---|---|
| [`plugin-api.md`](plugin-api.md) | Adding a plugin-facing interface. Records only what is **not yet implemented**, plus the capability decisions already settled (so they don't get re-proposed). Implemented behaviour lives in [`catalog/plugins/README.md`](../catalog/plugins/README.md). |
| [`windows-build.md`](windows-build.md) | Building, running or debugging on Windows. Temp-dir and launch-path constraints, broken task targets, the `AVChannelLayout` heap-corruption root cause, and stale conclusions not to repeat. |

Everything here is **verified on a real machine** — treat these as facts, not
suggestions, and update them in place when a fact changes (do not append a correction
below the stale claim).