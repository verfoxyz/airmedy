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
