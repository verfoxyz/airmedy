package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"airmedy/internal/domain"
)

// Registry limits. v1 keeps the numbers conservative on purpose: every running
// plugin is a child process, and a machine that runs out of file handles fails
// in ways that look like unrelated bugs.
const (
	defaultMaxRunning         = 8
	defaultMaxConcurrentCalls = 16
)

// Config configures the plugin registry.
type Config struct {
	// Root is the directory the registry scans for airmedy-plugin.json files.
	// An empty root disables discovery.
	Root string
	// MaxRunning caps how many plugin processes may run at once.
	MaxRunning int
	// MaxConcurrentCalls caps in-flight calls per plugin, so one plugin cannot
	// monopolise the transport or the host's goroutines.
	MaxConcurrentCalls int
	// LaunchEnv is consulted for extra environment variables per plugin at
	// launch time. A plugin whose content the user edits (the lx source list)
	// is told what to load through the environment rather than through a new
	// protocol method, and the registry reads it fresh on every start so a
	// change plus a restart is all it takes (see catalog/plugins/README.md,
	// "Lifecycle").
	//
	// It may be nil; a nil hook injects nothing.
	LaunchEnv func(ctx context.Context, pluginID string) []string
}

// normalise fills the zero values with the defaults.
func (c Config) normalise() Config {
	if c.MaxRunning <= 0 {
		c.MaxRunning = defaultMaxRunning
	}
	if c.MaxConcurrentCalls <= 0 {
		c.MaxConcurrentCalls = defaultMaxConcurrentCalls
	}
	return c
}

// PluginInfo is one registry entry as the UI sees it: what was installed and
// whether it is running now.
//
// One plugin is one functional unit: it owns its runtime, its capabilities and
// the providers it declares, and there is nothing below it. The UI therefore
// renders one row with one switch per entry.
type PluginInfo struct {
	Manifest     domain.PluginManifest
	Running      bool
	Capabilities []domain.Capability
	// Providers is the normalised routing declaration (the manifest's list).
	// Empty means the plugin serves no track source, which is legal.
	Providers []string
	// Enabled is the user's persisted choice. Use EffectiveEnabled for what the
	// app will actually run.
	Enabled bool
	// Status is the coarse state the UI shows.
	Status domain.PluginStatus
	// LastError explains a failed start or an invalid manifest. Empty when the
	// plugin is healthy.
	LastError string
	// Invalid marks a manifest that was discovered but cannot be used: the
	// manifest itself is malformed, it declares a provider another plugin also
	// declares, or it declares providers without stream.resolve. The entry
	// stays in the list so the user can see why it does not work.
	Invalid bool `json:"invalid"`
	// Settings is the switch list the running plugin reported through describe.
	// Empty means it declared none (or is not running).
	Settings []domain.PluginSettingDeclaration
}

// EffectiveEnabled reports whether the app will run this plugin.
func (i PluginInfo) EffectiveEnabled() bool {
	return i.Enabled && !i.Invalid
}

// entry is one registry slot: the manifest, its runtime state and the reason for
// the current state.
type entry struct {
	manifest domain.PluginManifest
	// enabled is the persisted user choice for this plugin id, before cascade.
	enabled bool
	// invalidReason is set when the manifest cannot be used at all (missing or
	// nested host). Such an entry is listed but never started.
	invalidReason error
	// lastErr is the most recent start failure, cleared on a successful start.
	lastErr error
	// started records whether a start was attempted and succeeded at least
	// once, so the UI can tell "never started" from "stopped".
	status domain.PluginStatus
	// describable records that the plugin answered describe at least once. For
	// those plugins dynamicProviders *is* the routing table, including "no
	// routes at all", so an empty list must not fall back to the manifest.
	describable bool
	// dynamicProviders is the last describe answer, normalised.
	dynamicProviders []string
	// dynamicSettings is the switch list the last describe answer declared. It
	// is cached so the settings row can render the controls without a live call.
	dynamicSettings []domain.PluginSettingDeclaration
}

// Service is the plugin registry: it discovers and validates manifests, owns the
// running hosts, and enforces the global and per-plugin concurrency caps.
//
// It depends only on domain ports (PluginLoader, PluginHostFactory and
// PluginSettingsRepository), so the app layer never imports the process
// implementation.
type Service struct {
	loader   domain.PluginLoader
	factory  domain.PluginHostFactory
	settings domain.PluginSettingsRepository
	logger   *slog.Logger
	config   Config

	mu      sync.Mutex
	entries map[string]*entry
	hosts   map[string]domain.PluginHost
	slots   map[string]chan struct{}
	closed  bool
}

// NewService builds the registry. The fx module starts it with the app and
// closes it on shutdown.
//
// settings may be nil in tests that do not exercise enable state; Start and
// Scan still work, but SetEnabled reports ErrPluginSettingsUnavailable.
func NewService(
	loader domain.PluginLoader,
	factory domain.PluginHostFactory,
	settings domain.PluginSettingsRepository,
	logger *slog.Logger,
	config Config,
) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		loader:   loader,
		factory:  factory,
		settings: settings,
		logger:   logger,
		config:   config.normalise(),
		entries:  make(map[string]*entry),
		hosts:    make(map[string]domain.PluginHost),
		slots:    make(map[string]chan struct{}),
	}
}

// Root returns the directory the registry scans for plugins. The settings page
// shows it so the user knows where to put a plugin folder; an empty root means
// discovery is disabled.
func (s *Service) Root() string {
	return s.config.Root
}

// Scan discovers and registers every manifest under the configured root, then
// reloads the persisted enable flags.
//
// A broken plugin must not stop the rest from loading: invalid manifests are
// logged and returned in the joined error, and the valid ones are still
// registered. Duplicate ids are rejected; the first manifest wins.
func (s *Service) Scan(ctx context.Context) ([]domain.PluginManifest, error) {
	if s.config.Root == "" {
		return nil, nil
	}
	paths, err := s.loader.Discover(s.config.Root)
	if err != nil {
		return nil, fmt.Errorf("discover plugins in %s: %w", s.config.Root, err)
	}

	// Collect everything first: provider conflicts are a property of the whole
	// installed set, so a manifest cannot be judged before its siblings are
	// known.
	found := make([]domain.PluginManifest, 0, len(paths))
	var errs []error
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		manifest, err := s.loader.Load(path)
		if err != nil {
			s.logger.Warn("Skipping plugin with an invalid manifest", "path", path, "error", err)
			errs = append(errs, err)
			continue
		}
		found = append(found, manifest)
	}

	registered, registerErrs := s.registerAll(found)
	errs = append(errs, registerErrs...)

	if err := s.loadEnabled(ctx); err != nil {
		s.logger.Warn("Failed to load plugin enable flags", "error", err)
		errs = append(errs, err)
	}

	if len(registered) > 0 {
		s.logger.Info("Registered plugins", "count", len(registered), "root", s.config.Root)
	}
	return registered, errors.Join(errs...)
}

// Load validates one manifest and registers it.
func (s *Service) Load(ctx context.Context, path string) (domain.PluginManifest, error) {
	if err := ctx.Err(); err != nil {
		return domain.PluginManifest{}, err
	}
	manifest, err := s.loader.Load(path)
	if err != nil {
		return domain.PluginManifest{}, err
	}

	s.mu.Lock()
	existing := s.snapshotManifestsLocked()
	s.mu.Unlock()

	// Re-resolving the whole set keeps the invariant "every provider has at
	// most one owner" true: a new claim can invalidate an already registered
	// plugin, so both sides are told about the conflict.
	all := append(existing, manifest)
	reasons := resolveProviders(all)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return domain.PluginManifest{}, domain.ErrPluginHostClosed
	}
	if _, exists := s.entries[manifest.ID]; exists {
		return domain.PluginManifest{}, fmt.Errorf("%w: %q", domain.ErrPluginAlreadyRegistered, manifest.ID)
	}
	s.insertLocked(manifest, reasons[manifest.ID])
	s.applyProviderReasonsLocked(reasons)
	return manifest, nil
}

// Register adds an already validated manifest to the registry.
//
// Provider claims are judged here because only the registry sees the whole
// installed set: a provider served by two plugins has no deterministic owner, so
// both entries are marked invalid and the error names the other plugin. That is
// the one rule a single manifest cannot check.
func (s *Service) Register(manifest domain.PluginManifest) error {
	s.mu.Lock()
	existing := s.snapshotManifestsLocked()
	s.mu.Unlock()

	all := append(existing, manifest)
	reasons := resolveProviders(all)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return domain.ErrPluginHostClosed
	}
	if _, exists := s.entries[manifest.ID]; exists {
		return fmt.Errorf("%w: %q", domain.ErrPluginAlreadyRegistered, manifest.ID)
	}
	s.insertLocked(manifest, reasons[manifest.ID])
	s.applyProviderReasonsLocked(reasons)
	return nil
}

// registerAll registers a batch, validating provider claims against the batch as
// a whole. Duplicates inside the batch and against the existing registry are
// rejected individually.
func (s *Service) registerAll(manifests []domain.PluginManifest) ([]domain.PluginManifest, []error) {
	var errs []error
	candidates := make([]domain.PluginManifest, 0, len(manifests))
	seen := make(map[string]struct{}, len(manifests))

	for _, manifest := range manifests {
		if _, duplicate := seen[manifest.ID]; duplicate {
			errs = append(errs, fmt.Errorf("%w: %q", domain.ErrPluginAlreadyRegistered, manifest.ID))
			s.logger.Warn("Skipping duplicate plugin", "plugin", manifest.ID)
			continue
		}
		seen[manifest.ID] = struct{}{}
		candidates = append(candidates, manifest)
	}

	s.mu.Lock()
	existing := s.snapshotManifestsLocked()
	s.mu.Unlock()
	candidates = append(candidates, existing...)
	reasons := resolveProviders(candidates)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, []error{domain.ErrPluginHostClosed}
	}

	registered := make([]domain.PluginManifest, 0, len(manifests))
	for _, manifest := range manifests {
		if _, duplicate := s.entries[manifest.ID]; duplicate {
			errs = append(errs, fmt.Errorf("%w: %q", domain.ErrPluginAlreadyRegistered, manifest.ID))
			continue
		}
		s.insertLocked(manifest, reasons[manifest.ID])
		registered = append(registered, manifest)
	}
	// Conflicts can also hit plugins registered by an earlier scan; refresh
	// those reasons instead of leaving a stale "valid" entry behind.
	s.applyProviderReasonsLocked(reasons)
	return registered, errs
}

// insertLocked adds one entry. The caller holds s.mu.
func (s *Service) insertLocked(manifest domain.PluginManifest, invalidReason error) {
	e := &entry{
		manifest:      manifest,
		status:        domain.PluginStatusIdle,
		invalidReason: invalidReason,
	}
	s.entries[manifest.ID] = e
	s.slots[manifest.ID] = make(chan struct{}, s.config.MaxConcurrentCalls)

	if e.invalidReason != nil {
		s.logger.Warn("Plugin is installed but unavailable",
			"plugin", manifest.ID, "error", e.invalidReason)
	}
}

// applyProviderReasonsLocked refreshes the stored invalid reason of already
// registered entries from a freshly computed conflict map. The caller holds
// s.mu.
//
// Only provider-derived reasons are touched, in both directions: a new conflict
// marks an entry invalid, and a conflict that disappeared (the other plugin was
// removed) clears the reason again. A start failure and a reason for an entry
// that is not in the map are left alone, so a rescan never erases why a plugin
// failed to launch.
func (s *Service) applyProviderReasonsLocked(reasons map[string]error) {
	for id, e := range s.entries {
		reason, conflicted := reasons[id]
		switch {
		case conflicted:
			if !errors.Is(e.invalidReason, reason) {
				s.logger.Warn("Plugin is installed but unavailable",
					"plugin", id, "error", reason)
			}
			e.invalidReason = reason
		case isProviderReason(e.invalidReason):
			e.invalidReason = nil
		}
	}
}

// isProviderReason reports whether err came from the provider rules, so a stale
// conflict can be cleared without touching other invalidation causes.
func isProviderReason(err error) bool {
	return errors.Is(err, domain.ErrPluginProviderConflict) ||
		errors.Is(err, domain.ErrPluginProviderWithoutCapability)
}

// snapshotManifestsLocked copies the registered manifests. The caller holds
// s.mu.
func (s *Service) snapshotManifestsLocked() []domain.PluginManifest {
	manifests := make([]domain.PluginManifest, 0, len(s.entries))
	for _, e := range s.entries {
		manifests = append(manifests, e.manifest)
	}
	return manifests
}

// resolveProviders validates every provider claim in one pass over the whole
// set.
//
// It returns the reason each unusable plugin was rejected, keyed by plugin id; a
// plugin with no entry is usable. The rules:
//
//   - a plugin declaring providers must declare stream.resolve, otherwise the
//     route it advertises can never be served;
//   - a provider may have exactly one owner, so every plugin claiming a
//     duplicated id is rejected and the message names both sides.
//
// The check is order-independent on purpose: discovery order must not decide
// which of two conflicting plugins wins, because the user would have no way to
// predict the winner.
func resolveProviders(manifests []domain.PluginManifest) map[string]error {
	// First registration wins, mirroring the registry's duplicate-id rule, so
	// the reason is computed over the same set the registry will keep.
	byID := make(map[string]domain.PluginManifest, len(manifests))
	order := make([]string, 0, len(manifests))
	for _, manifest := range manifests {
		if _, exists := byID[manifest.ID]; exists {
			continue
		}
		byID[manifest.ID] = manifest
		order = append(order, manifest.ID)
	}

	reasons := make(map[string]error)
	owners := make(map[string][]string, len(order))
	for _, id := range order {
		manifest := byID[id]
		if len(manifest.Providers) == 0 {
			continue
		}
		if !domain.HasCapability(manifest.Capabilities, domain.CapabilityStreamResolve) {
			reasons[id] = fmt.Errorf("%w: plugin %q lists providers %v",
				domain.ErrPluginProviderWithoutCapability, id, manifest.Providers)
			continue
		}
		for _, provider := range manifest.Providers {
			owners[provider] = append(owners[provider], id)
		}
	}

	// Walk the providers in declaration order so the message reads the same way
	// on every run.
	for _, id := range order {
		manifest := byID[id]
		for _, provider := range manifest.Providers {
			claimants := owners[provider]
			if len(claimants) < 2 {
				continue
			}
			others := make([]string, 0, len(claimants)-1)
			for _, claimant := range claimants {
				if claimant != id {
					others = append(others, claimant)
				}
			}
			reasons[id] = fmt.Errorf("%w: provider %q is also declared by %s",
				domain.ErrPluginProviderConflict, provider, strings.Join(others, ", "))
			break
		}
	}
	return reasons
}

// ReloadEnabled re-reads the persisted flags and applies them to the registry.
//
// Scan calls it, and the settings page calls it after a plugin directory change
// so a newly installed plugin's stored flag takes effect without an app restart.
func (s *Service) ReloadEnabled(ctx context.Context) error {
	return s.loadEnabled(ctx)
}

// loadEnabled reads the persisted flags into the registry. A missing row means
// "not enabled": plugins are off until the user turns them on.
func (s *Service) loadEnabled(ctx context.Context) error {
	if s.settings == nil {
		return nil
	}
	rows, err := s.settings.List(ctx)
	if err != nil {
		return err
	}
	enabled := make(map[string]bool, len(rows))
	for _, row := range rows {
		enabled[row.PluginID] = row.Enabled
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.entries {
		e.enabled = enabled[id]
	}
	return nil
}

// List returns every registered plugin, sorted by id. Capabilities come from the
// running host when there is one (the handshake is authoritative) and from the
// manifest otherwise.
func (s *Service) List() []PluginInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

// listLocked builds the UI view. The caller holds s.mu.
func (s *Service) listLocked() []PluginInfo {
	ids := make([]string, 0, len(s.entries))
	for id := range s.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	infos := make([]PluginInfo, 0, len(ids))
	for _, id := range ids {
		e := s.entries[id]
		info := PluginInfo{
			Manifest:  e.manifest,
			Enabled:   e.enabled,
			Status:    e.status,
			Providers: append([]string(nil), e.liveProvidersLocked()...),
			Settings:  append([]domain.PluginSettingDeclaration(nil), e.dynamicSettings...),
		}
		if host, running := s.hosts[id]; running {
			info.Running = true
			info.Capabilities = host.Capabilities()
			info.Status = domain.PluginStatusRunning
		} else {
			info.Capabilities = append([]domain.Capability(nil), e.manifest.Capabilities...)
		}
		if e.invalidReason != nil {
			info.Invalid = true
			info.LastError = e.invalidReason.Error()
			info.Status = domain.PluginStatusFailed
		} else if e.lastErr != nil {
			info.LastError = e.lastErr.Error()
			if !info.Running {
				info.Status = domain.PluginStatusFailed
			}
		}
		infos = append(infos, info)
	}
	return infos
}

// HostForProvider returns the running plugin that declared provider, or false
// when no plugin claims it.
//
// This is the routing table the stream resolver consumes: the answer comes from
// the manifests, so a request reaches exactly one plugin and no plugin is ever
// asked "is this yours?". An invalid or stopped plugin is not returned — routing
// to it would turn a missing install into a confusing call failure.
func (s *Service) HostForProvider(ctx context.Context, provider string) (domain.PluginHost, bool) {
	if err := ctx.Err(); err != nil {
		return nil, false
	}
	provider = domain.NormalizeProvider(provider)
	if provider == "" {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Walk the entries in id order. The ownership rule makes at most one entry
	// claim a provider, but a map iteration would still pick nondeterministically
	// in the window where a new claim has not been conflict-checked yet, and a
	// flapping owner is far harder to debug than a sorted walk costs.
	for _, id := range s.sortedEntryIDsLocked() {
		e := s.entries[id]
		if e.invalidReason != nil || !s.claimsProviderLocked(e, provider) {
			continue
		}
		if host, running := s.hosts[id]; running {
			return host, true
		}
	}
	return nil, false
}

// sortedEntryIDsLocked returns the registered plugin ids in a stable order. The
// caller holds s.mu.
func (s *Service) sortedEntryIDsLocked() []string {
	ids := make([]string, 0, len(s.entries))
	for id := range s.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// HostForLyricSource returns the running plugin that declared provider under
// `sources` and implements lyric.fetch.
//
// Lyrics route by the manifest source list rather than by `providers`: the
// search plugin never claims a stream route, but it is exactly the plugin that
// knows the platform's lyric endpoint for the rows it produced.
func (s *Service) HostForLyricSource(ctx context.Context, provider string) (domain.PluginHost, bool) {
	if err := ctx.Err(); err != nil {
		return nil, false
	}
	provider = domain.NormalizeProvider(provider)
	if provider == "" {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.sortedEntryIDsLocked() {
		e := s.entries[id]
		if e.invalidReason != nil {
			continue
		}
		if !domain.HasCapability(e.manifest.Capabilities, domain.CapabilityLyricFetch) {
			continue
		}
		declared := false
		for _, source := range e.manifest.Sources {
			if domain.NormalizeProvider(source) == provider {
				declared = true
				break
			}
		}
		if !declared {
			continue
		}
		if host, running := s.hosts[id]; running {
			return host, true
		}
	}
	return nil, false
}

// Providers returns provider → plugin id for every registered plugin that claims
// it, including plugins that are not running. The settings page shows the
// ownership and the stream router's error messages name it.
func (s *Service) Providers() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	owners := make(map[string]string)
	for id, e := range s.entries {
		if e.invalidReason != nil {
			continue
		}
		for _, provider := range e.liveProvidersLocked() {
			owners[domain.NormalizeProvider(provider)] = id
		}
	}
	return owners
}

// Describe asks a running plugin for its current routes and sub-sources.
//
// The answer is adopted as the plugin's dynamic routing declaration, replacing
// whatever an earlier describe reported. It is the hook the lx plugin uses to
// tell the app which scripts it actually loaded, so the user can add or disable
// a source without the app having to know anything about lx (see §2.4 of the
// task).
//
// A plugin that does not implement describe answers -32601 and keeps its static
// manifest routes; that is "not supported", not a failure, so the sentinel is
// returned unwrapped and every caller treats it accordingly.
func (s *Service) Describe(ctx context.Context, id string) (domain.PluginDescribe, error) {
	var describe domain.PluginDescribe
	if err := s.Call(ctx, id, domain.MethodDescribe, nil, &describe); err != nil {
		return domain.PluginDescribe{}, err
	}

	providers := normaliseProviderList(describe.Providers)
	if describe.Sources == nil {
		describe.Sources = []domain.PluginSourceDescription{}
	}
	describe.Providers = providers
	if describe.Settings == nil {
		describe.Settings = []domain.PluginSettingDeclaration{}
	}

	s.mu.Lock()
	if e, ok := s.entries[id]; ok {
		e.dynamicProviders = providers
		e.dynamicSettings = append([]domain.PluginSettingDeclaration(nil), describe.Settings...)
		e.describable = true
	}
	s.mu.Unlock()

	return describe, nil
}

// RefreshDynamic re-runs describe for every plugin that has answered it once,
// and recomputes the provider claims.
//
// The user's source list lives outside the plugin's manifest, so a change (add,
// remove, toggle, reorder) can only be reflected by restarting the plugin and
// asking again. The caller does the restart; this applies the new routes to the
// already running host.
func (s *Service) RefreshDynamic(ctx context.Context) error {
	s.mu.Lock()
	ids := make([]string, 0, len(s.entries))
	for id, e := range s.entries {
		if !e.describable || e.invalidReason != nil {
			continue
		}
		if _, running := s.hosts[id]; !running {
			continue
		}
		ids = append(ids, id)
	}
	s.mu.Unlock()
	sort.Strings(ids)

	var errs []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if _, err := s.Describe(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("refresh routes of plugin %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// Restart stops and starts one plugin, then re-asks for its routes.
//
// Restarting is deliberate: the lx plugin reads its source list from the
// environment at launch, so a change in that list needs a fresh process. A
// hot-reload protocol would be more code for a ~200ms stutter the user caused
// themselves by saving.
func (s *Service) Restart(ctx context.Context, id string) error {
	// A restart is the host's own decision (a source list change), which is
	// exactly what `reload` distinguishes from the user switching the plugin off.
	if err := s.StopWithReason(ctx, id, domain.PluginUnloadReasonReload); err != nil {
		return err
	}
	if err := s.Start(ctx, id); err != nil {
		return err
	}
	return s.RefreshDynamic(ctx)
}

// claimsProviderLocked reports whether e declares provider. The caller holds
// s.mu.
//
// The comparison normalises the declared id as well, even though the manifest
// loader already did: Register accepts a manifest from any caller (tests, a
// future importer), and a hand-written " WY " must not become an unroutable
// route.
func (s *Service) claimsProviderLocked(e *entry, provider string) bool {
	for _, declared := range e.liveProvidersLocked() {
		if domain.NormalizeProvider(declared) == provider {
			return true
		}
	}
	return false
}

// liveProvidersLocked returns the routes a plugin currently serves. The caller
// holds s.mu.
//
// A plugin that answered describe owns its routes, including the case where the
// answer was "none". Between a start and the describe that follows it, a
// describable plugin reports nothing: serving the stale list of the *previous*
// process would route a request into a process that does not know that source
// yet, and the stream resolver would then fail it for the wrong reason. The
// window is one round-trip wide and a request simply reports the provider as
// unowned, which is what it is at that moment.
func (e *entry) liveProvidersLocked() []string {
	if e.describable {
		return e.dynamicProviders
	}
	return e.manifest.Providers
}

// Capabilities returns what the plugin declared: the live handshake result when
// it is running, the manifest otherwise.
func (s *Service) Capabilities(id string) ([]domain.Capability, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if host := s.hosts[id]; host != nil {
		return host.Capabilities(), nil
	}
	e, ok := s.entries[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", domain.ErrPluginNotFound, id)
	}
	return append([]domain.Capability(nil), e.manifest.Capabilities...), nil
}

// Status returns the coarse lifecycle state of one plugin and the most recent
// failure reason, if any.
func (s *Service) Status(id string) (domain.PluginStatus, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[id]
	if !ok {
		return "", "", fmt.Errorf("%w: %q", domain.ErrPluginNotFound, id)
	}
	if _, running := s.hosts[id]; running {
		return domain.PluginStatusRunning, "", nil
	}
	if e.invalidReason != nil {
		return domain.PluginStatusFailed, e.invalidReason.Error(), nil
	}
	if e.lastErr != nil {
		return domain.PluginStatusFailed, e.lastErr.Error(), nil
	}
	return domain.PluginStatusIdle, "", nil
}

// Host returns the running host for id.
//
// It deliberately does not start a stopped plugin: callers sit on playback and
// download paths where a process launch plus a handshake would block a user
// action, so starting a plugin stays an explicit decision.
func (s *Service) Host(ctx context.Context, id string) (domain.PluginHost, bool) {
	if err := ctx.Err(); err != nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	host, ok := s.hosts[id]
	return host, ok
}

// RunningHosts returns every running plugin, sorted by id.
//
// It is the coarse "what is up" view (tests, diagnostics, the settings page's
// badge); routing never walks it, because a provider's owner is declared in the
// manifest and HostForProvider answers that question in one lookup.
func (s *Service) RunningHosts(ctx context.Context) []domain.PluginHost {
	if err := ctx.Err(); err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.hosts))
	for id := range s.hosts {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	hosts := make([]domain.PluginHost, 0, len(ids))
	for _, id := range ids {
		hosts = append(hosts, s.hosts[id])
	}
	return hosts
}

// Start launches a registered plugin unless it is already running. Start is
// idempotent; concurrent callers cannot end up with two processes for one id.
func (s *Service) Start(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return domain.ErrPluginHostClosed
	}
	e, ok := s.entries[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %q", domain.ErrPluginNotFound, id)
	}
	if e.invalidReason != nil {
		s.mu.Unlock()
		return e.invalidReason
	}
	if _, running := s.hosts[id]; running {
		s.mu.Unlock()
		return nil
	}
	if len(s.hosts) >= s.config.MaxRunning {
		s.mu.Unlock()
		return fmt.Errorf("%w: %d plugins already running", domain.ErrPluginRunningLimit, s.config.MaxRunning)
	}
	manifest := e.manifest
	s.mu.Unlock()

	// The manifest is the launch input; the per-plugin environment is read
	// again on every start, so a caller that changed the plugin's content only
	// has to restart it (Config.LaunchEnv).
	if s.config.LaunchEnv != nil {
		manifest.Runtime.Env = mergeEnv(manifest.Runtime.Env, s.config.LaunchEnv(ctx, id))
	}

	// Launch outside the lock: starting a process and finishing the handshake
	// takes long enough that holding the registry would block every reader.
	host, err := s.factory(ctx, manifest)
	if err != nil {
		s.recordStartFailure(id, err)
		return fmt.Errorf("start plugin %q: %w", id, err)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = host.Close()
		return domain.ErrPluginHostClosed
	}
	if _, running := s.hosts[id]; running {
		// Another Start won the race; keep the first host and drop this one.
		s.mu.Unlock()
		if err := host.Close(); err != nil {
			s.logger.Warn("Failed to close duplicate plugin host", "plugin", id, "error", err)
		}
		return nil
	}
	s.hosts[id] = host
	if e, ok := s.entries[id]; ok {
		e.status = domain.PluginStatusRunning
		e.lastErr = nil
		// Fail closed until the describe below answers: a plugin whose routes
		// come from describe must not be routed on the *previous* process's
		// list while the new process is still being asked.
		if e.describable {
			e.dynamicProviders = nil
			e.dynamicSettings = nil
		}
	}
	s.mu.Unlock()

	// A dynamic plugin rebuilds its routes from content it just loaded, so the
	// manifest declaration is not authoritative for it. Asking here (rather
	// than only on demand) means a freshly started plugin is immediately
	// routable; a plugin that does not implement describe answers -32601 and
	// this is a no-op.
	s.describeQuietly(ctx, id)
	return nil
}

// describeQuietly asks a plugin for its dynamic routes after a start, ignoring
// both "not supported" and a failure: a plugin that cannot describe still works
// through its manifest declaration, and a start must not be undone by a
// bookkeeping call.
func (s *Service) describeQuietly(ctx context.Context, id string) {
	if _, err := s.Describe(ctx, id); err != nil {
		// -32601 is the normal "this plugin has static routes" answer, so it is
		// not worth a log line; anything else is worth a debug entry.
		if errors.Is(err, domain.ErrPluginMethodNotFound) {
			return
		}
		s.logger.Debug("Plugin did not describe its routes", "plugin", id, "error", err)
	}
}

// mergeEnv layers extra KEY=VALUE pairs over a manifest's environment map.
func mergeEnv(base map[string]string, extra []string) map[string]string {
	if len(extra) == 0 {
		return base
	}
	merged := make(map[string]string, len(base)+len(extra))
	for key, value := range base {
		merged[key] = value
	}
	for _, entry := range extra {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		merged[key] = value
	}
	return merged
}

// normaliseProviderList trims, lower-cases, de-duplicates and drops blanks in a
// provider list a plugin reported. The plugin is third-party code; the app must
// not route on an unnormalised id or on an empty string.
func normaliseProviderList(providers []string) []string {
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

// recordStartFailure stores why a start failed so List/Status can show it. One
// bad plugin must never stop the app or hide the others.
func (s *Service) recordStartFailure(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[id]; ok {
		e.status = domain.PluginStatusFailed
		e.lastErr = err
	}
}

// Stop closes a running plugin. Stopping a plugin that is not running is a no-op
// so a repeated user action cannot fail.
//
// The default reason is "disabled": the registry cannot know whether the caller
// is a user toggle, a restart or app shutdown, so it assumes the least surprising
// one. StopWithReason states it explicitly, which is what Restart and StopAll do.
func (s *Service) Stop(ctx context.Context, id string) error {
	return s.StopWithReason(ctx, id, domain.PluginUnloadReasonDisabled)
}

// StopWithReason closes a running plugin and tells it why (lifecycle.unload,
// docs/plugin-api.md). The reason reaches the plugin as a hint to flush state;
// it never changes the escalation the host performs.
func (s *Service) StopWithReason(ctx context.Context, id string, reason domain.PluginUnloadReason) error {
	_ = ctx // Close owns its shutdown grace; the context only marks caller intent.
	s.mu.Lock()
	host, running := s.hosts[id]
	if running {
		delete(s.hosts, id)
	}
	if e, ok := s.entries[id]; ok && e.status == domain.PluginStatusRunning {
		e.status = domain.PluginStatusIdle
	}
	s.mu.Unlock()

	if !running {
		return nil
	}
	if err := unloadPlugin(host, reason); err != nil {
		return fmt.Errorf("stop plugin %q: %w", id, err)
	}
	return nil
}

// PermitsKV reports whether plugin id declared `permissions.kv`, which is the
// right `storage.*` requests are checked against.
//
// It reads the live entry rather than the host's manifest copy so the answer
// follows a rescan: a plugin updated to declare the permission is served without
// an app restart. An unknown id has no entry and therefore no permission, which
// is the fail-closed default the KV handler relies on.
func (s *Service) PermitsKV(pluginID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[pluginID]
	if !ok {
		return false
	}
	return e.manifest.Permissions.KV
}

// unloadPlugin tells an implementation that supports the notification why it is
// being reclaimed, and falls back to Close for one that does not.
//
// PluginUnloader is an optional interface so an existing PluginHost
// implementation (a test fake, an older adapter) keeps working; a plugin that
// cannot hear the reason is in exactly the position it was in before the
// notification existed.
func unloadPlugin(host domain.PluginHost, reason domain.PluginUnloadReason) error {
	if unloader, ok := host.(domain.PluginUnloader); ok {
		return unloader.Unload(reason)
	}
	return host.Close()
}

// Call invokes a method on a running plugin, honouring the per-plugin concurrency
// cap. A plugin that is registered but not started returns ErrPluginNotRunning;
// an unknown id returns ErrPluginNotFound.
func (s *Service) Call(ctx context.Context, id, method string, params, result any) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return domain.ErrPluginHostClosed
	}
	host := s.hosts[id]
	slots := s.slots[id]
	_, known := s.entries[id]
	s.mu.Unlock()

	if host == nil {
		if !known {
			return fmt.Errorf("%w: %q", domain.ErrPluginNotFound, id)
		}
		return fmt.Errorf("plugin %q: %w", id, domain.ErrPluginNotRunning)
	}
	if err := acquireSlot(ctx, slots); err != nil {
		return fmt.Errorf("plugin %q call %q: %w", id, method, err)
	}
	defer func() { <-slots }()

	return host.Call(ctx, method, params, result)
}

// StopAll closes every running plugin. Ids stay registered so the registry can be
// inspected after shutdown.
func (s *Service) StopAll(ctx context.Context) error {
	_ = ctx
	s.mu.Lock()
	hosts := s.hosts
	s.hosts = make(map[string]domain.PluginHost)
	for _, e := range s.entries {
		if e.status == domain.PluginStatusRunning {
			e.status = domain.PluginStatusIdle
		}
	}
	s.mu.Unlock()

	var errs []error
	for id, host := range hosts {
		// StopAll is the app exiting, so the plugins get `shutdown` rather than
		// the default: a plugin may want to flush something different when the
		// whole app is going away.
		if err := unloadPlugin(host, domain.PluginUnloadReasonShutdown); err != nil {
			errs = append(errs, fmt.Errorf("stop plugin %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// Close stops every plugin and refuses further registry mutations. It is
// idempotent and is what the fx lifecycle calls on shutdown.
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.StopAll(ctx)
}

// SetEnabled persists the user's choice for id and applies it immediately:
// enabling starts the plugin, disabling stops it. One plugin is one functional
// unit, so there is nothing else whose state has to follow.
func (s *Service) SetEnabled(ctx context.Context, id string, enabled bool) error {
	if s.settings == nil {
		return domain.ErrPluginSettingsUnavailable
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return domain.ErrPluginHostClosed
	}
	e, ok := s.entries[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %q", domain.ErrPluginNotFound, id)
	}
	if e.invalidReason != nil {
		s.mu.Unlock()
		return e.invalidReason
	}
	s.mu.Unlock()

	if err := s.settings.Set(ctx, id, enabled); err != nil {
		return fmt.Errorf("set plugin %q enabled=%v: %w", id, enabled, err)
	}

	s.mu.Lock()
	if e, ok := s.entries[id]; ok {
		e.enabled = enabled
	}
	s.mu.Unlock()

	if enabled {
		if err := s.Start(ctx, id); err != nil {
			return err
		}
		return nil
	}
	return s.Stop(ctx, id)
}

// StartEnabled launches every plugin the user enabled.
//
// One failing plugin must not stop the application from opening: failures are
// recorded on the entry (status failed + reason) and returned joined. The
// running cap still applies, so a user with more enabled plugins than the cap
// gets a clear error instead of a machine full of processes.
func (s *Service) StartEnabled(ctx context.Context) error {
	s.mu.Lock()
	ids := make([]string, 0, len(s.entries))
	for id, e := range s.entries {
		if !e.enabled || e.invalidReason != nil {
			continue
		}
		ids = append(ids, id)
	}
	s.mu.Unlock()
	sort.Strings(ids)

	var errs []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := s.Start(ctx, id); err != nil {
			s.logger.Warn("Failed to start enabled plugin", "plugin", id, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// acquireSlot waits for a per-plugin call slot or for the caller to give up.
func acquireSlot(ctx context.Context, slots chan struct{}) error {
	if slots == nil {
		return nil
	}
	select {
	case slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
