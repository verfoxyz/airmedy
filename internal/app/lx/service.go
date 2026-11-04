package lx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"

	"airmedy/internal/domain"
)

// Import limits and constants.
const (
	// defaultMaxSources bounds the list. Every source is a sandbox inside one
	// plugin process, so an unbounded list is both a memory problem and a UI
	// the user cannot manage.
	defaultMaxSources = 20

	// SourcesEnvVar is the environment variable the core injects into the Lx
	// plugin at launch. The value is a JSON array of the enabled sources in try
	// order, so the plugin needs no protocol method to learn what to load
	// (see catalog/plugins/README.md, "Managed source lists").
	SourcesEnvVar = "AIRMEDY_LX_SOURCES"

	// PluginID is the plugin whose content this service manages. It is data
	// here: the core stores files and a list, and implements no lx semantics.
	PluginID = "lx"
)

// Config configures the lx source service.
type Config struct {
	// SourcesDir is where imported scripts are copied. main.go sets it to
	// <DataDir>/plugins/lx/sources, next to the plugin it belongs to.
	SourcesDir string
	// MaxSources caps the list; zero means defaultMaxSources.
	MaxSources int
}

func (c Config) normalise() Config {
	if c.MaxSources <= 0 {
		c.MaxSources = defaultMaxSources
	}
	return c
}

// PluginRegistry is the slice of the plugin registry this service needs.
//
// It is defined here rather than imported from app/plugin so the two app
// services stay independent and this package can be tested without a registry.
// *plugin.Service satisfies it.
type PluginRegistry interface {
	// Host returns the running plugin for id, or false when it is not running.
	Host(ctx context.Context, id string) (domain.PluginHost, bool)
	// Start launches a plugin that is registered but idle.
	Start(ctx context.Context, id string) error
	// Stop closes a running plugin.
	Stop(ctx context.Context, id string) error
	// Restart stops and starts a plugin, then refreshes its dynamic routes.
	Restart(ctx context.Context, id string) error
	// Describe asks a running plugin for its current routes and sources.
	Describe(ctx context.Context, id string) (domain.PluginDescribe, error)
	// Call invokes one method on a running plugin.
	Call(ctx context.Context, id, method string, params, result any) error
}

// Service owns the imported lx source list: the files on disk, the database
// rows, the order, and the environment the plugin is launched with.
//
// It implements no lx semantics at all. Whether a file is a usable custom source
// is answered by the plugin (`source.validate`), because only the real lx
// runtime can decide that; the service keeps the cheap policy (file type, size,
// list cap, error wording) and the lifecycle around the check.
type Service struct {
	repo   domain.LxSourceRepository
	logger *slog.Logger
	config Config

	// mu guards registry, which is attached after construction to keep the fx
	// graph acyclic: the registry needs the launch-environment hook this service
	// provides, so it cannot also be a constructor argument.
	mu       sync.RWMutex
	registry PluginRegistry
}

// NewService builds the lx source service.
//
// The plugin registry is attached with AttachRegistry rather than injected here,
// because the registry consumes this service's LaunchEnv: passing it to the
// constructor would close a dependency cycle.
func NewService(repo domain.LxSourceRepository, logger *slog.Logger, config Config) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		repo:   repo,
		logger: logger,
		config: config.normalise(),
	}
}

// AttachRegistry sets the registry used to restart the plugin after a change and
// to read its describe answer.
//
// main.go attaches it once during startup, before any plugin is launched; tests
// may attach a fake at any point. A nil registry is legal and turns every
// plugin-facing action into ErrLxPluginUnavailable rather than a panic.
func (s *Service) AttachRegistry(registry PluginRegistry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry = registry
}

// registryRef returns the attached registry, or nil.
func (s *Service) registryRef() PluginRegistry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.registry
}

// SourcesDir returns the directory scripts are copied into. The settings page
// shows it so the user can find the copies.
func (s *Service) SourcesDir() string {
	return s.config.SourcesDir
}

// List returns the stored sources in try order.
func (s *Service) List(ctx context.Context) ([]domain.LxSource, error) {
	sources, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	if sources == nil {
		sources = []domain.LxSource{}
	}
	return sources, nil
}

// Import copies one script into the sources directory and registers it.
//
// The copy is deliberate: users move and rename the file they downloaded, and a
// stored path to the original would break playback the moment they tidy their
// Downloads folder. The new row is appended after every existing source, so an
// import never silently outranks a source the user already trusts.
func (s *Service) Import(ctx context.Context, path string) (domain.LxSource, error) {
	sources, err := s.repo.List(ctx)
	if err != nil {
		return domain.LxSource{}, err
	}
	if len(sources) >= s.config.MaxSources {
		return domain.LxSource{}, fmt.Errorf("%w: %d sources are already imported",
			domain.ErrLxSourceLimit, len(sources))
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return domain.LxSource{}, fmt.Errorf("%w: resolve %s: %v", domain.ErrLxSourceInvalid, path, err)
	}
	if err := checkScriptFile(absolute); err != nil {
		return domain.LxSource{}, err
	}

	// A uuid file name: importing the same file twice yields two independent
	// rows with two independent copies, which is what "import it twice" means
	// to the user (acceptance case 4).
	id := uuid.NewString()
	if err := os.MkdirAll(s.config.SourcesDir, 0o755); err != nil {
		return domain.LxSource{}, fmt.Errorf("create lx sources directory: %w", err)
	}
	destination := filepath.Join(s.config.SourcesDir, id+domain.LxSourceExtension)
	data, err := os.ReadFile(absolute)
	if err != nil {
		return domain.LxSource{}, fmt.Errorf("%w: read %s: %v", domain.ErrLxSourceInvalid, absolute, err)
	}
	if err := os.WriteFile(destination, data, 0o644); err != nil {
		return domain.LxSource{}, fmt.Errorf("store lx source %s: %w", destination, err)
	}

	// Validation runs on the *copy*, through the plugin's own sandbox. The core
	// implements no lx semantics, and asking the plugin is also the only way to
	// be certain that a script the app stores plays back: the same runtime that
	// rejected it here is the one that would have failed later, at play time.
	validated, err := s.validate(ctx, destination)
	if err != nil {
		_ = os.Remove(destination)
		return domain.LxSource{}, err
	}

	name := strings.TrimSpace(validated.Name)
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(absolute), filepath.Ext(absolute))
	}

	source := domain.LxSource{
		ID:       id,
		Name:     name,
		FilePath: destination,
		Enabled:  true,
		Position: nextPosition(sources),
	}
	if err := s.repo.Create(ctx, source); err != nil {
		// The copy would be orphaned if the row cannot be written; remove it so
		// a failed import leaves no invisible file behind.
		_ = os.Remove(destination)
		return domain.LxSource{}, err
	}
	s.logger.Info("Imported lx source",
		"id", id, "name", name, "providers", validated.Providers, "path", destination)

	s.reload(ctx)
	return source, nil
}

// SetEnabled toggles one source and restarts the plugin when the state changes.
func (s *Service) SetEnabled(ctx context.Context, id string, enabled bool) error {
	sources, err := s.repo.List(ctx)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if source.ID != id {
			continue
		}
		if source.Enabled == enabled {
			// Nothing to restart: the plugin already runs with this state.
			return nil
		}
		source.Enabled = enabled
		if err := s.repo.Update(ctx, source); err != nil {
			return err
		}
		s.reload(ctx)
		return nil
	}
	return fmt.Errorf("%w: %q", domain.ErrLxSourceNotFound, id)
}

// Rename changes the display label of one source.
//
// It restarts nothing: the name is a UI label, and the plugin learns file
// contents, not labels.
func (s *Service) Rename(ctx context.Context, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: a source needs a name", domain.ErrLxSourceInvalid)
	}
	sources, err := s.repo.List(ctx)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if source.ID != id {
			continue
		}
		source.Name = name
		return s.repo.Update(ctx, source)
	}
	return fmt.Errorf("%w: %q", domain.ErrLxSourceNotFound, id)
}

// Remove deletes one source, its stored file and the plugin's route for it.
func (s *Service) Remove(ctx context.Context, id string) error {
	sources, err := s.repo.List(ctx)
	if err != nil {
		return err
	}
	var (
		found bool
		path  string
	)
	for _, source := range sources {
		if source.ID == id {
			found = true
			path = source.FilePath
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: %q", domain.ErrLxSourceNotFound, id)
	}
	if err := s.repo.Delete(ctx, id); err != nil {
		return err
	}
	// The row is the source of truth; a file that cannot be removed is a
	// leftover, not something the user can act on, so it is logged only.
	if path != "" && withinDir(path, s.config.SourcesDir) {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			s.logger.Warn("Could not remove the stored lx source file", "path", path, "error", removeErr)
		}
	}
	s.reload(ctx)
	return nil
}

// Reorder rewrites the try order and restarts the plugin.
//
// ids is the complete list in the new order. Unknown ids are ignored by the
// repository, so a stale UI list cannot fail the whole operation.
func (s *Service) Reorder(ctx context.Context, ids []string) error {
	if err := s.repo.Reorder(ctx, ids); err != nil {
		return err
	}
	s.reload(ctx)
	return nil
}

// Describe returns the plugin's view of its sources.
//
// The plugin is authoritative about what actually loaded and which platforms
// each script registered; the stored rows say what the user wants. The UI joins
// the two, and the join is visible in the DTO rather than hidden here.
func (s *Service) Describe(ctx context.Context) (domain.PluginDescribe, error) {
	registry := s.registryRef()
	if registry == nil {
		return domain.PluginDescribe{}, fmt.Errorf("%w: no plugin registry is bound", domain.ErrLxPluginUnavailable)
	}
	if _, running := registry.Host(ctx, PluginID); !running {
		return domain.PluginDescribe{}, fmt.Errorf(
			"%w: the lx plugin is not running (enable it in Settings → Plugins)", domain.ErrLxPluginUnavailable)
	}
	return registry.Describe(ctx, PluginID)
}

// LaunchEnv returns the environment the registry injects when it starts the
// plugin: the enabled sources in try order, as JSON.
//
// The format is flat and self-describing; the plugin parses it with JSON.parse
// and needs no schema file. Disabled sources are omitted rather than flagged so
// the plugin cannot accidentally serve one.
func (s *Service) LaunchEnv(ctx context.Context) ([]string, error) {
	sources, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]launchSource, 0, len(sources))
	for _, source := range sources {
		if !source.Enabled {
			continue
		}
		entries = append(entries, launchSource{
			ID:       source.ID,
			Name:     source.Name,
			Path:     source.FilePath,
			Enabled:  true,
			Position: source.Position,
		})
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("encode lx source list: %w", err)
	}
	return []string{SourcesEnvVar + "=" + string(encoded)}, nil
}

// launchSource is the JSON object in AIRMEDY_LX_SOURCES. It mirrors
// domain.LxSource but is a wire shape, so it lives next to the encoder.
type launchSource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Enabled  bool   `json:"enabled"`
	Position int    `json:"position"`
}

// validate asks the running plugin to load a candidate script in a throwaway
// sandbox and report what it registered.
//
// The plugin is started for the duration of the check when it is not already
// running: importing a source is a user action on the same plugin they manage,
// and answering "turn the plugin on first" to a click on "Add source" would be
// a worse experience than a 200ms start. The plugin is stopped again afterwards
// so a manager that was idle does not become running because someone imported a
// file.
func (s *Service) validate(ctx context.Context, path string) (domain.PluginSourceValidation, error) {
	registry := s.registryRef()
	if registry == nil {
		return domain.PluginSourceValidation{}, fmt.Errorf(
			"%w: no plugin registry is bound", domain.ErrLxPluginUnavailable)
	}

	started, err := s.ensureRunning(ctx, registry)
	if err != nil {
		return domain.PluginSourceValidation{}, err
	}
	defer func() {
		if started {
			if stopErr := registry.Stop(context.WithoutCancel(ctx), PluginID); stopErr != nil {
				s.logger.Warn("Failed to stop the lx plugin after validation", "error", stopErr)
			}
		}
	}()

	var result domain.PluginSourceValidation
	err = registry.Call(ctx, PluginID, domain.MethodValidateSource,
		domain.PluginValidateSourceRequest{Path: path}, &result)
	if err != nil {
		var rpcErr *domain.RPCError
		if errors.As(err, &rpcErr) {
			return domain.PluginSourceValidation{}, fmt.Errorf("%w: %s", domain.ErrLxSourceInvalid, rpcErr.Message)
		}
		return domain.PluginSourceValidation{}, fmt.Errorf(
			"%w: the Lx plugin could not check the file: %v", domain.ErrLxPluginUnavailable, err)
	}

	providers := normaliseProviders(result.Providers)
	if len(providers) == 0 {
		return domain.PluginSourceValidation{}, fmt.Errorf(
			"%w: %s does not register any platform, so it is not an lx custom source",
			domain.ErrLxSourceInvalid, filepath.Base(path))
	}
	result.Providers = providers
	return result, nil
}

// ensureRunning starts the plugin when needed and reports whether it did, so the
// caller knows whether to stop it again.
func (s *Service) ensureRunning(ctx context.Context, registry PluginRegistry) (bool, error) {
	if _, running := registry.Host(ctx, PluginID); running {
		return false, nil
	}
	if err := registry.Start(ctx, PluginID); err != nil {
		return false, fmt.Errorf("%w: the Lx plugin could not be started: %v", domain.ErrLxPluginUnavailable, err)
	}
	return true, nil
}

// reload restarts the plugin so it picks up the new list.
//
// Restarting is the contract (task §3): the plugin reads the list from its
// environment at launch, so writing the list and restarting is the whole update.
// A plugin that is not running is not an error — a user may manage the list with
// the plugin disabled, and the next start picks the list up anyway.
func (s *Service) reload(ctx context.Context) {
	registry := s.registryRef()
	if registry == nil {
		return
	}
	if _, running := registry.Host(ctx, PluginID); !running {
		return
	}
	if err := registry.Restart(ctx, PluginID); err != nil {
		s.logger.Warn("Failed to restart the lx plugin after a source change", "error", err)
	}
}

// checkScriptFile enforces the cheap rules before the runtime is involved: the
// extension, the file type, and the size.
func checkScriptFile(path string) error {
	if strings.ToLower(filepath.Ext(path)) != domain.LxSourceExtension {
		return fmt.Errorf("%w: %s is not a .js file", domain.ErrLxSourceInvalid, filepath.Base(path))
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: read %s: %v", domain.ErrLxSourceInvalid, filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", domain.ErrLxSourceInvalid, filepath.Base(path))
	}
	if info.Size() > domain.MaxLxSourceBytes {
		return fmt.Errorf("%w: %s is %d bytes, larger than the %d byte limit",
			domain.ErrLxSourceInvalid, filepath.Base(path), info.Size(), domain.MaxLxSourceBytes)
	}
	return nil
}

// nextPosition returns the position that puts a new source after every existing
// one.
func nextPosition(sources []domain.LxSource) int {
	position := 0
	for _, source := range sources {
		if source.Position >= position {
			position = source.Position + 1
		}
	}
	return position
}

// withinDir reports whether path is inside dir, so a hand-edited database row
// cannot make the service delete an arbitrary file.
func withinDir(path, dir string) bool {
	if dir == "" {
		return false
	}
	relative, err := filepath.Rel(dir, path)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// normaliseProviders applies the same rule as the registry: trim, lower-case,
// drop blanks and duplicates. The plugin does it too; doing it here keeps the
// label list and the routing table from disagreeing on case.
func normaliseProviders(providers []string) []string {
	seen := make(map[string]struct{}, len(providers))
	normalised := make([]string, 0, len(providers))
	for _, provider := range providers {
		value := domain.NormalizeProvider(provider)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		normalised = append(normalised, value)
	}
	return normalised
}
