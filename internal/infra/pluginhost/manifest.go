package pluginhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/blang/semver"

	"airmedy/internal/domain"
)

// maxManifestSize bounds a manifest file. A manifest is a small hand-written
// document; anything larger is a mistake (or an attack) and is rejected before
// it is decoded.
const maxManifestSize = 1 << 20 // 1 MiB

// pluginIDPattern keeps ids usable as directory and cache names later. The id
// is also the registry key, so it must be stable and free of path syntax.
var pluginIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// LoadManifest reads, parses and validates one airmedy-plugin.json.
//
// Paths inside the manifest resolve relative to the manifest directory: the
// command is made absolute when it looks like a path (`./bin/plugin`), while a
// bare command (`node`) is left for the OS to look up in PATH.
func LoadManifest(path string) (domain.PluginManifest, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return domain.PluginManifest{}, fmt.Errorf("%w: resolve %s: %v",
			domain.ErrPluginManifestInvalid, path, err)
	}
	file, err := os.Open(absolute)
	if err != nil {
		return domain.PluginManifest{}, fmt.Errorf("%w: open %s: %v",
			domain.ErrPluginManifestInvalid, absolute, err)
	}
	// Read-only: a close failure cannot change what was parsed, and the file
	// descriptor is released either way.
	defer func() { _ = file.Close() }()

	manifest, err := ParseManifest(io.LimitReader(file, maxManifestSize))
	if err != nil {
		return domain.PluginManifest{}, fmt.Errorf("%s: %w", absolute, err)
	}
	manifest.Dir = filepath.Dir(absolute)
	if command := manifest.Runtime.Command; command != "" && !filepath.IsAbs(command) && looksLikePath(command) {
		manifest.Runtime.Command = filepath.Join(manifest.Dir, command)
	}
	return manifest, nil
}

// ParseManifest decodes and validates a manifest from r.
func ParseManifest(r io.Reader) (domain.PluginManifest, error) {
	var manifest domain.PluginManifest
	decoder := json.NewDecoder(r)
	if err := decoder.Decode(&manifest); err != nil {
		return domain.PluginManifest{}, fmt.Errorf("%w: decode: %v", domain.ErrPluginManifestInvalid, err)
	}
	if err := validateManifest(&manifest); err != nil {
		return domain.PluginManifest{}, err
	}
	return manifest, nil
}

// validateManifest applies the v1 manifest rules and normalises the fields the
// rest of the host relies on. It never panics on missing or malformed input:
// every path returns an error wrapping ErrPluginManifestInvalid.
func validateManifest(manifest *domain.PluginManifest) error {
	manifest.ID = strings.TrimSpace(manifest.ID)
	if manifest.ID == "" {
		return fmt.Errorf("%w: id is required", domain.ErrPluginManifestInvalid)
	}
	if !pluginIDPattern.MatchString(manifest.ID) {
		return fmt.Errorf("%w: id %q must match %s", domain.ErrPluginManifestInvalid, manifest.ID, pluginIDPattern)
	}

	manifest.Name = strings.TrimSpace(manifest.Name)
	if manifest.Name == "" {
		// A missing display name is not fatal: the id is a usable fallback and
		// rejecting the plugin would only hide it from the user.
		manifest.Name = manifest.ID
	}

	manifest.Version = strings.TrimSpace(manifest.Version)
	if manifest.Version != "" {
		if _, err := semver.ParseTolerant(manifest.Version); err != nil {
			return fmt.Errorf("%w: version %q is not semver: %v",
				domain.ErrPluginManifestInvalid, manifest.Version, err)
		}
	}

	// Author is display-only and optional: a plugin without one is still
	// installable, so it is trimmed rather than required.
	manifest.Author = strings.TrimSpace(manifest.Author)

	// apiVersion is required and must be a range, not a bare feature flag:
	// the handshake needs something to negotiate against.
	manifest.APIVersion = strings.TrimSpace(manifest.APIVersion)
	if manifest.APIVersion == "" {
		return fmt.Errorf("%w: apiVersion is required", domain.ErrPluginManifestInvalid)
	}
	if _, err := parseAPIRange(manifest.APIVersion); err != nil {
		return err
	}

	// providers is the routing declaration: one id per track source the plugin
	// serves. Blank entries and duplicates are dropped, case and padding are
	// folded by domain.NormalizeProvider so a manifest cannot declare " WY " and
	// "wy" as two routes. The cross-plugin conflict rule lives in the registry,
	// because no single file can see the other plugins.
	manifest.Providers = normaliseProviders(manifest.Providers)

	manifest.Runtime.Type = strings.TrimSpace(manifest.Runtime.Type)
	if manifest.Runtime.Type == "" {
		return fmt.Errorf("%w: runtime.type is required", domain.ErrPluginManifestInvalid)
	}
	if !strings.EqualFold(manifest.Runtime.Type, domain.RuntimeTypeProcess) {
		return fmt.Errorf("%w: runtime.type %q is not supported (only %q)",
			domain.ErrPluginManifestInvalid, manifest.Runtime.Type, domain.RuntimeTypeProcess)
	}
	manifest.Runtime.Type = domain.RuntimeTypeProcess
	manifest.Runtime.Command = strings.TrimSpace(manifest.Runtime.Command)
	if manifest.Runtime.Command == "" {
		return fmt.Errorf("%w: runtime.command is required", domain.ErrPluginManifestInvalid)
	}

	// Unknown capabilities are kept; only the obvious mistakes (blank entries)
	// are dropped. See domain.IsKnownCapability for why this is not a whitelist.
	seen := make(map[domain.Capability]struct{}, len(manifest.Capabilities))
	capabilities := make([]domain.Capability, 0, len(manifest.Capabilities))
	for _, capability := range manifest.Capabilities {
		capability = domain.Capability(strings.TrimSpace(string(capability)))
		if capability == "" {
			continue
		}
		if _, duplicate := seen[capability]; duplicate {
			continue
		}
		seen[capability] = struct{}{}
		capabilities = append(capabilities, capability)
	}
	manifest.Capabilities = capabilities

	// Providers are only meaningful next to stream.resolve: without the
	// capability nothing would ever call the plugin for a track, so the
	// declaration would advertise a route that cannot be served.
	if len(manifest.Providers) > 0 && !domain.HasCapability(capabilities, domain.CapabilityStreamResolve) {
		return fmt.Errorf("%w: providers %v require the %q capability",
			domain.ErrPluginManifestInvalid, manifest.Providers, domain.CapabilityStreamResolve)
	}

	network := make([]string, 0, len(manifest.Permissions.Network))
	for _, host := range manifest.Permissions.Network {
		if host = strings.TrimSpace(host); host != "" {
			network = append(network, host)
		}
	}
	manifest.Permissions.Network = network

	return nil
}

// normaliseProviders folds a declared provider list into the canonical form the
// router looks up: trimmed, lower-cased, blank-free and deduplicated, keeping the
// author's order so the settings page shows what the manifest says.
func normaliseProviders(providers []string) []string {
	normalised := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		provider = domain.NormalizeProvider(provider)
		if provider == "" {
			continue
		}
		if _, duplicate := seen[provider]; duplicate {
			continue
		}
		seen[provider] = struct{}{}
		normalised = append(normalised, provider)
	}
	if len(normalised) == 0 {
		// nil, not [], so the manifest stays "serves no provider" instead of
		// claiming an empty (and therefore guaranteed-failing) route set.
		return nil
	}
	return normalised
}

// looksLikePath distinguishes an explicit relative path (`./plugin`,
// `bin/plugin`) from a command name meant for PATH (`node`, `python3`).
func looksLikePath(command string) bool {
	if strings.ContainsAny(command, `/\`) {
		return true
	}
	return strings.HasPrefix(command, ".")
}

// DiscoverManifests finds every airmedy-plugin.json under dir.
//
// The directory is created by the app on first run; a missing dir simply means
// no plugins are installed. Hidden subdirectories are skipped because they are
// where caches live, not where a user drops a plugin.
func DiscoverManifests(dir string) ([]string, error) {
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect plugin directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("plugin path %s is not a directory", dir)
	}

	var paths []string
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != dir && strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() == domain.ManifestFileName {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan plugin directory %s: %w", dir, err)
	}
	sort.Strings(paths)
	return paths, nil
}

// Loader is the disk-backed domain.PluginLoader.
type Loader struct {
	logger *slog.Logger
}

// NewLoader returns the manifest loader used by the plugin registry.
func NewLoader(logger *slog.Logger) *Loader {
	if logger == nil {
		logger = slog.Default()
	}
	return &Loader{logger: logger}
}

// Discover implements domain.PluginLoader.
func (l *Loader) Discover(dir string) ([]string, error) {
	return DiscoverManifests(dir)
}

// Load implements domain.PluginLoader.
func (l *Loader) Load(path string) (domain.PluginManifest, error) {
	manifest, err := LoadManifest(path)
	if err != nil {
		return domain.PluginManifest{}, err
	}
	logUnknownCapabilities(l.logger, manifest.ID, manifest.Capabilities)
	return manifest, nil
}

// logUnknownCapabilities records capabilities the host cannot use yet. They are
// not an error, but a silent unknown would make a plugin look partially broken
// with no trace to explain it.
func logUnknownCapabilities(logger *slog.Logger, pluginID string, capabilities []domain.Capability) {
	for _, capability := range capabilities {
		if domain.IsKnownCapability(capability) {
			continue
		}
		logger.Debug("Plugin declares a capability the host does not know",
			"plugin", pluginID, "capability", string(capability))
	}
}
