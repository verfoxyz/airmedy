# Plugins

Plugins are **not distributed with this repository**. The app loads them at runtime from
its data directory:

```
<data dir>/plugins/<plugin-id>/
  airmedy-plugin.json     manifest: id, runtime, capabilities, providers, permissions
  wrapper.js ...          whatever the manifest's runtime command needs
```

On Windows the data directory is `%LOCALAPPDATA%\airmedy`; on macOS/Linux it is the app's
XDG data directory. Install a plugin by copying its folder there, then enable it in
Settings → Plugins and rescan.

The contract a plugin implements — manifest fields, JSON-RPC methods, host methods
(`storage.*`), permissions and lifecycle — is documented in
[`catalog/plugins/README.md`](../catalog/plugins/README.md).

This folder is intentionally empty in git: a plugin that talks to a third-party service is
installed and maintained by the user, so it is neither shipped nor versioned here.

## Where the plugin sources live

Each plugin is its own repository outside this workspace, and the data directory holds a
**copy** of it:

| Plugin | Source repository | Deployed to |
|---|---|---|
| `wy`（聚合搜索） | `D:\code\music\wy` | `<DataDir>\plugins\wy\` |
| `lx`（Lx 自定义源） | `D:\code\music\lx` | `<DataDir>\plugins\lx\` |

Deploy after changing either repository:

```bash
scripts/deploy-plugins.sh
```

Then restart Airmedy: the plugin host starts the child processes at launch and does not
reload a manifest in place.

Do not replace `<DataDir>/plugins/<id>/` with a junction or symlink — plugin discovery uses
`filepath.WalkDir`, which does not follow reparse points, so a linked directory is silently
skipped and the plugin never registers. The copy has to be real files.
