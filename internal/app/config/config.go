package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrg/xdg"
)

type Config struct {
	DataDir string
}

func NewConfig() (*Config, error) {
	dataDir := filepath.Join(xdg.DataHome, appDataFolder)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}
	cfg := &Config{DataDir: dataDir}
	// The plugin directory is created up front so the settings page can name a
	// real path in its empty state. A failure here is not fatal: the user can
	// still create it, and the registry treats a missing directory as "no
	// plugins installed".
	if err := os.MkdirAll(cfg.PluginsDir(), 0755); err != nil {
		return nil, fmt.Errorf("failed to create plugin directory: %w", err)
	}
	return cfg, nil
}

// PluginsDir is where airmedy-plugin.json directories are installed.
//
// It is created eagerly so the settings page can tell the user where to put a
// plugin instead of pointing at a path that does not exist yet.
func (c *Config) PluginsDir() string {
	return filepath.Join(c.DataDir, "plugins")
}

// LxSourcesDir is where imported lx custom-source scripts are copied. The lx
// plugin lives in <DataDir>/plugins/lx/, so its user content sits next to it and
// a plugin upgrade (which replaces the plugin directory) cannot delete it.
func (c *Config) LxSourcesDir() string {
	return filepath.Join(c.PluginsDir(), "lx", "sources")
}

func (c *Config) DBPath() string {
	return filepath.Join(c.DataDir, "airmedy.db")
}

func (c *Config) IndexPath() string {
	return filepath.Join(c.DataDir, "airmedy.bleve")
}

func (c *Config) ArtworkCachePath() string {
	return filepath.Join(c.DataDir, "artwork")
}

// CoverCachePath is the persistent remote-cover directory. It is separate from
// the artwork cache because its files are addressed by remote URL hash and
// evicted by an LRU index, while artwork files are addressed by content hash.
func (c *Config) CoverCachePath() string {
	return filepath.Join(c.DataDir, "cover-cache")
}

func (c *Config) LogDir() string {
	return filepath.Join(c.DataDir, "logs")
}

func (c *Config) LogPath() string {
	return filepath.Join(c.LogDir(), "airmedy.log")
}
