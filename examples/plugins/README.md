# Example plugin

One leaf plugin that implements `stream.resolve` and declares the provider it
serves. It is **not built by the app** and **not installed by default**; it
exists so the routing and lifecycle contracts can be run and asserted end to end.

| File | Purpose |
|---|---|
| `airmedy-plugin.json` | Manifest: `capabilities: ["stream.resolve"]` and `providers: ["echo.test"]` |
| `main.go` | The plugin itself: newline-delimited JSON-RPC on stdio, one fixed URL |

## The point of the example

There is exactly one directory and one manifest. The plugin owns its own
implementation, and the app learns everything it needs from the manifest:

- `capabilities` says what the plugin can do;
- `providers` says which track sources it serves;
- `runtime` says how to launch it.

The app never asks a plugin whether a provider is "its", so there is no decline
message in the protocol. A provider nobody declares is a routing miss the app
reports itself, with the provider named.

## Install it

The app scans `<DataDir>/plugins/` recursively. One plugin is one directory:

```
<DataDir>/plugins/
└── echo/
    ├── airmedy-plugin.json   # id: echo, providers: [echo.test]
    └── echo(.exe)            # built binary
```

1. Build the plugin next to its manifest:

   ```bash
   go build -o examples/plugins/echo/echo ./examples/plugins/echo
   ```

2. Copy that directory into `<DataDir>/plugins/echo/`.

3. Restart the app, then enable **Echo Source** in Settings → Plugins. It appears
   as one row with its own switch.

`<DataDir>` is `%LOCALAPPDATA%\airmedy` on Windows, `~/Library/Application
Support/airmedy` on macOS and `~/.local/share/airmedy` on Linux.

## Poke at it by hand

```bash
cd examples/plugins
go build -o echo/echo ./echo

printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"hostVersion":"1.0","apiVersion":"1.0","locale":"en"}}' \
  '{"jsonrpc":"2.0","id":2,"method":"stream.resolve","params":{"source":"echo.test","id":"x","quality":"320k"}}' \
  '{"jsonrpc":"2.0","id":3,"method":"stream.resolve","params":{"source":"nope","id":"x","quality":"320k"}}' \
  '{"jsonrpc":"2.0","id":4,"method":"shutdown"}' \
  | ./echo/echo
```

Answer 2 carries the fixed URL. Answer 3 is an `unavailable` failure with a
structured kind — the app routes by declaration, so it never sends a provider the
plugin did not list. Answer 4 exits.

## Tests

`internal/infra/pluginhost/example_plugins_test.go` builds the plugin into a temp
directory and drives it through the real host implementation (manifest load,
process launch, handshake, `stream.resolve`). The tests skip — rather than fail —
when the Go toolchain cannot build the example, so they never mask a real failure
on a machine without CGO.