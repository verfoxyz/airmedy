-- Plugin key/value storage (see domain.PluginKVRepository).
--
-- The host owns this store because a plugin is a child process with no
-- persistence: whatever it wants to remember between restarts has to live
-- somewhere, and the alternative — a plugin writing into the app database —
-- would give third-party code a schema it must not see.
--
-- Isolation is by plugin_id, which is the plugin id from its manifest and the
-- same value the registry keys everything else on. One row per (plugin, key):
-- a `storage.set` is an upsert, so a plugin can rewrite the same key without a
-- delete first.
--
-- value is opaque TEXT: the plugin's own JSON, never parsed by the host. The
-- 64 KiB per-value cap and the per-plugin key count are enforced in
-- app/pluginkv rather than by a CHECK constraint, because a limit that changes
-- should not need a migration to raise.
CREATE TABLE plugin_kv (
    plugin_id   TEXT NOT NULL,
    key         TEXT NOT NULL,
    value       TEXT NOT NULL,
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (plugin_id, key)
);
