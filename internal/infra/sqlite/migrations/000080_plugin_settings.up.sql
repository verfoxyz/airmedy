-- Per-plugin enable flags (see domain.PluginSettings).
--
-- A missing row means "not enabled". Plugins are off until the user turns them
-- on, because enabling one launches a third-party child process; storing only
-- the enabled rows means a fresh install never starts anything unexpected.
--
-- One row per plugin, and a plugin is one functional unit: there is no parent
-- row that overrides it, so the schema needs no hierarchy. Re-scanning the
-- plugin directory keeps rows for plugins that are no longer installed; the
-- registry ignores unknown ids rather than deleting user state.
CREATE TABLE plugin_settings (
    plugin_id  TEXT PRIMARY KEY,
    enabled    INTEGER NOT NULL DEFAULT 0,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
