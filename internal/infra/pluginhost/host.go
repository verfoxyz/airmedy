package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"airmedy/internal/domain"
)

// Protocol timings and the restart budget from the v1 contract.
const (
	defaultHandshakeTimeout = 10 * time.Second
	defaultCallTimeout      = 15 * time.Second
	defaultShutdownGrace    = 3 * time.Second
	defaultMaxRestarts      = 3
)

// Options tunes one host instance. The zero value is not ready to use: build it
// with defaultOptions and the With* helpers.
type Options struct {
	// Logger receives host events and forwarded plugin stderr.
	Logger *slog.Logger
	// HostVersion is the concrete API version reported in initialize.
	HostVersion string
	// Locale is forwarded to the plugin so it can localise its own strings.
	Locale string
	// Env is appended to the plugin's environment, after the manifest's env.
	Env []string
	// HandshakeTimeout bounds initialize.
	HandshakeTimeout time.Duration
	// CallTimeout is the default deadline for one Call; a caller context with
	// an earlier deadline wins.
	CallTimeout time.Duration
	// ShutdownGrace is how long the host waits for a plugin to exit before
	// escalating: shutdown request → stdin EOF → terminate → kill.
	ShutdownGrace time.Duration
	// MaxRestarts caps automatic relaunches after a crash.
	MaxRestarts int
	// Handler answers plugin → host requests (domain.PluginKV today). A nil
	// handler keeps the v1 answer for every inbound method: -32601.
	Handler domain.PluginRequestHandler
}

func defaultOptions() Options {
	return Options{
		HostVersion:      domain.HostAPIVersion,
		Locale:           "en",
		HandshakeTimeout: defaultHandshakeTimeout,
		CallTimeout:      defaultCallTimeout,
		ShutdownGrace:    defaultShutdownGrace,
		MaxRestarts:      defaultMaxRestarts,
	}
}

// Option customises a host.
type Option func(*Options)

// WithLogger attaches the host logger. It must not be nil when the option is
// used.
func WithLogger(logger *slog.Logger) Option {
	return func(options *Options) { options.Logger = logger }
}

// WithHostVersion overrides the API version reported to plugins. Tests use it
// to exercise negotiation; production keeps domain.HostAPIVersion.
func WithHostVersion(version string) Option {
	return func(options *Options) { options.HostVersion = version }
}

// WithLocale sets the locale forwarded to the plugin.
func WithLocale(locale string) Option {
	return func(options *Options) { options.Locale = locale }
}

// WithEnv appends environment variables (in KEY=VALUE form) to the plugin.
func WithEnv(env ...string) Option {
	return func(options *Options) { options.Env = append(options.Env, env...) }
}

// WithHandshakeTimeout overrides the initialize deadline.
func WithHandshakeTimeout(timeout time.Duration) Option {
	return func(options *Options) { options.HandshakeTimeout = timeout }
}

// WithCallTimeout overrides the default per-call deadline.
func WithCallTimeout(timeout time.Duration) Option {
	return func(options *Options) { options.CallTimeout = timeout }
}

// WithShutdownGrace overrides how long Close waits before escalating.
func WithShutdownGrace(grace time.Duration) Option {
	return func(options *Options) { options.ShutdownGrace = grace }
}

// WithMaxRestarts overrides how many crash relaunches are allowed.
func WithMaxRestarts(count int) Option {
	return func(options *Options) { options.MaxRestarts = count }
}

// WithRequestHandler binds the plugin → host request set. Without it the host
// answers every inbound method -32601, which is the v1 contract.
func WithRequestHandler(handler domain.PluginRequestHandler) Option {
	return func(options *Options) { options.Handler = handler }
}

// Host is one running plugin process that speaks the v1 stdio protocol.
//
// Lifecycle: Start launches the process and completes initialize; Call speaks
// the protocol; Close asks the plugin to exit and reclaims it. A crashed process
// is not restarted behind the caller's back: the failing Call returns -32000 and
// the next Call - or an explicit restart via Start - relaunches it, up to
// MaxRestarts times in the host's lifetime.
type Host struct {
	manifest domain.PluginManifest
	options  Options
	logger   *slog.Logger

	// opMu serialises launch, restart and close so two callers cannot race a
	// process hand-off.
	opMu sync.Mutex
	// mu guards the state below. It is never held while waiting on the plugin.
	mu           sync.Mutex
	proc         *process
	transport    *transport
	capabilities []domain.Capability
	running      bool
	closed       bool
	restarts     int

	watchers  sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
	// closeReason is what Unload was told, kept for tests and diagnostics.
	closeReason domain.PluginUnloadReason
}

// Start loads the manifest at manifestPath, launches the plugin, performs the
// initialize handshake and returns a ready host.
//
// A plugin that cannot be loaded, does not answer the handshake, or claims an
// apiVersion the host cannot speak is never returned half-started: the process
// is reclaimed before the error is returned.
func Start(ctx context.Context, manifestPath string, options ...Option) (*Host, error) {
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	return StartManifest(ctx, manifest, options...)
}

// StartManifest starts a plugin from an already loaded manifest. It is the entry
// point the app registry uses, because the registry validates and de-duplicates
// manifests before deciding which ones to run.
func StartManifest(ctx context.Context, manifest domain.PluginManifest, options ...Option) (*Host, error) {
	opts := defaultOptions()
	for _, apply := range options {
		apply(&opts)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	host := &Host{
		manifest: manifest,
		options:  opts,
		logger:   opts.Logger,
	}
	logUnknownCapabilities(host.logger, manifest.ID, manifest.Capabilities)
	if err := host.launch(ctx); err != nil {
		return nil, err
	}
	return host, nil
}

// Manifest implements domain.PluginHost.
func (h *Host) Manifest() domain.PluginManifest {
	return h.manifest
}

// Capabilities implements domain.PluginHost. It returns what the plugin
// answered during the handshake, not what the manifest claimed: the handshake is
// the evidence that the plugin can actually serve those capabilities.
func (h *Host) Capabilities() []domain.Capability {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]domain.Capability(nil), h.capabilities...)
}

// Running reports whether the plugin process is currently serving.
func (h *Host) Running() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}

// Call implements domain.PluginHost.
func (h *Host) Call(ctx context.Context, method string, params, result any) error {
	if err := h.ensureRunning(ctx); err != nil {
		return err
	}

	h.mu.Lock()
	transport := h.transport
	h.mu.Unlock()
	if transport == nil {
		return fmt.Errorf("plugin call %q: %w", method, domain.ErrPluginCrashed)
	}

	callCtx, cancel := context.WithTimeout(ctx, h.options.CallTimeout)
	defer cancel()
	err := transport.call(callCtx, method, params, result)
	if err != nil {
		// A call that failed because the process died must mark the host
		// stopped, so the next call attempts a controlled relaunch instead of
		// writing into a dead pipe. The transport usually learns about the
		// exit before cmd.Wait does, so the crash sentinel is the reliable
		// signal here.
		//
		// A well-formed error *reply* is not a crash, even though the protocol
		// maps -32000 to the same sentinel: plugins use -32000 for ordinary
		// failures (a decline, a source error). Treating that as a death marked
		// a healthy plugin stopped on every refused request and forced a
		// relaunch. An RPCError means the plugin answered; only transport
		// errors mean it is gone.
		var rpcErr *domain.RPCError
		answered := errors.As(err, &rpcErr)
		h.mu.Lock()
		if !answered && (errors.Is(err, domain.ErrPluginCrashed) || (h.proc != nil && h.proc.exited())) {
			h.running = false
		}
		h.mu.Unlock()
	}
	return err
}

// Close implements domain.PluginHost. It is safe to call more than once and
// never returns before the child process and its goroutines are gone.
//
// Close is "the host is done with you" without a stated reason; the registry
// uses Unload when it knows one, so a plugin can tell a reload from a shutdown.
func (h *Host) Close() error {
	return h.Unload(domain.PluginUnloadReasonDisabled)
}

// Unload implements domain.PluginUnloader: it notifies the plugin that it is
// being reclaimed and why, then performs the Close escalation.
//
// The notification is best effort. A plugin that does not understand it ignores
// it (it is a notification, so nothing waits for an answer), a plugin that has
// already died cannot receive it, and a plugin that answers nothing still gets
// the same shutdown → EOF → terminate → kill ladder. The reason is therefore a
// hint that lets a careful plugin flush state, never a synchronisation point.
func (h *Host) Unload(reason domain.PluginUnloadReason) error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closeReason = reason
		h.mu.Unlock()
		h.closeErr = h.shutdown(reason)
	})
	return h.closeErr
}

// CloseReason reports the reason the host was closed with, for diagnostics. It
// is empty before Close/Unload ran.
func (h *Host) CloseReason() domain.PluginUnloadReason {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closeReason
}

// ensureRunning relaunches a crashed plugin when the restart budget allows it.
func (h *Host) ensureRunning(ctx context.Context) error {
	h.opMu.Lock()
	defer h.opMu.Unlock()

	h.mu.Lock()
	closed, running, restarts := h.closed, h.running, h.restarts
	proc, transport := h.proc, h.transport
	h.mu.Unlock()

	if closed {
		return domain.ErrPluginHostClosed
	}
	// `running` alone is not proof of life: the watcher and the reader can both
	// be a hair behind a dead process, so check the session and the process too.
	if running && proc != nil && !proc.exited() && transport != nil && !transport.isClosed() {
		return nil
	}
	if restarts >= h.options.MaxRestarts {
		return fmt.Errorf("plugin %q: %w (restarted %d times)",
			h.manifest.ID, domain.ErrPluginUnavailable, restarts)
	}

	h.mu.Lock()
	h.restarts++
	h.mu.Unlock()
	return h.launchLocked(ctx)
}

// launch starts a fresh process and handshake. It takes opMu itself; callers
// that already hold it use launchLocked.
func (h *Host) launch(ctx context.Context) error {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	return h.launchLocked(ctx)
}

// launchLocked replaces the current process with a fresh one. The caller holds
// opMu.
func (h *Host) launchLocked(ctx context.Context) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return domain.ErrPluginHostClosed
	}
	previousTransport := h.transport
	previousProcess := h.proc
	h.mu.Unlock()

	// A relaunch can only follow a crash, but the previous process may still be
	// alive briefly; make sure it is gone before starting the replacement so the
	// two never share stdin.
	if previousTransport != nil {
		previousTransport.close(domain.ErrPluginCrashed)
	}
	if previousProcess != nil {
		previousProcess.kill()
		previousProcess.waitExit(h.options.ShutdownGrace)
		previousProcess.waitWorkers()
		if previousTransport != nil {
			previousTransport.waitReader()
		}
	}

	proc, err := startProcess(h.manifest, h.logger, h.options.Env)
	if err != nil {
		return err
	}

	transport := newTransport(proc.stdout, proc.stdin, h.manifest.ID, h.logger, h.options.Handler)
	capabilities, err := h.handshake(ctx, transport)
	if err != nil {
		transport.close(err)
		proc.kill()
		proc.waitExit(h.options.ShutdownGrace)
		proc.waitWorkers()
		transport.waitReader()
		return err
	}

	h.mu.Lock()
	h.proc = proc
	h.transport = transport
	h.capabilities = capabilities
	h.running = true
	h.mu.Unlock()

	h.watchers.Add(1)
	go h.watch(proc)
	return nil
}

// watch flips the host to "not running" when the process exits. It returns as
// soon as the process is gone, so Close can wait for it without leaking.
func (h *Host) watch(proc *process) {
	defer h.watchers.Done()
	<-proc.done
	h.mu.Lock()
	if h.proc == proc {
		h.running = false
	}
	h.mu.Unlock()
}

// shutdown performs the graceful-stop escalation from the contract: shutdown
// request, stdin EOF, terminate, kill.
func (h *Host) shutdown(reason domain.PluginUnloadReason) error {
	h.opMu.Lock()
	defer h.opMu.Unlock()

	h.mu.Lock()
	h.closed = true
	h.running = false
	h.closeReason = reason
	transport := h.transport
	proc := h.proc
	h.mu.Unlock()

	if proc != nil {
		if !proc.exited() && transport != nil {
			// Tell the plugin *why* before the process is reclaimed. It is a
			// notification, and it goes out before the shutdown request so an
			// unaware plugin loses nothing: it still receives `shutdown` next.
			if err := transport.notify(domain.MethodLifecycleUnload, domain.PluginUnloadParams{Reason: reason}); err != nil {
				h.logger.Debug("Failed to send lifecycle.unload",
					"plugin", h.manifest.ID, "reason", reason, "error", err)
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), h.options.ShutdownGrace)
			_ = transport.call(shutdownCtx, "shutdown", nil, nil)
			cancel()
		}
		proc.closeStdin()
		if !proc.waitExit(h.options.ShutdownGrace) {
			proc.terminate()
			if !proc.waitExit(h.options.ShutdownGrace) {
				proc.kill()
				proc.waitExit(0)
			}
		}
		proc.waitWorkers()
	}
	if transport != nil {
		transport.close(domain.ErrPluginHostClosed)
		if proc != nil {
			transport.waitReader()
		}
	}
	h.watchers.Wait()
	return nil
}

// initializeParams is the host side of the handshake from the v1 contract.
type initializeParams struct {
	HostVersion string `json:"hostVersion"`
	APIVersion  string `json:"apiVersion"`
	Locale      string `json:"locale"`
}

// initializeResult is the plugin side of the handshake. ui is deliberately not
// modelled in v1: the host does not render plugin UI yet, and ignoring an
// unknown field keeps newer plugins loadable.
type initializeResult struct {
	APIVersion   string              `json:"apiVersion"`
	Capabilities []domain.Capability `json:"capabilities"`
}

// handshake performs initialize and negotiates the API version.
func (h *Host) handshake(parent context.Context, transport *transport) ([]domain.Capability, error) {
	ctx, cancel := context.WithTimeout(parent, h.options.HandshakeTimeout)
	defer cancel()

	var result initializeResult
	err := transport.call(ctx, "initialize", initializeParams{
		HostVersion: h.options.HostVersion,
		APIVersion:  domain.HostAPIVersion,
		Locale:      h.options.Locale,
	}, &result)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("%w: plugin %q did not answer initialize within %s",
				domain.ErrPluginHandshakeTimeout, h.manifest.ID, h.options.HandshakeTimeout)
		case errors.Is(err, context.Canceled) && parent.Err() != nil:
			// The caller gave up before the plugin did; that is not a protocol
			// failure of the plugin.
			return nil, fmt.Errorf("plugin %q handshake: %w", h.manifest.ID, parent.Err())
		default:
			return nil, fmt.Errorf("plugin %q handshake: %w", h.manifest.ID, err)
		}
	}

	if result.APIVersion == "" {
		return nil, fmt.Errorf("%w: plugin %q answered initialize without apiVersion",
			domain.ErrPluginHandshakeTimeout, h.manifest.ID)
	}

	satisfied, err := apiVersionSatisfies(h.manifest.APIVersion, result.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("plugin %q handshake: %w", h.manifest.ID, err)
	}
	if !satisfied {
		return nil, fmt.Errorf("%w: plugin %q speaks %q, manifest accepts %q",
			domain.ErrPluginIncompatibleAPIVersion, h.manifest.ID, result.APIVersion, h.manifest.APIVersion)
	}
	if !apiVersionCompatible(result.APIVersion, h.options.HostVersion) {
		return nil, fmt.Errorf("%w: plugin %q speaks %q, host speaks %q",
			domain.ErrPluginIncompatibleAPIVersion, h.manifest.ID, result.APIVersion, h.options.HostVersion)
	}

	capabilities := normaliseCapabilities(result.Capabilities)
	if len(capabilities) == 0 {
		// A plugin that answers with no capabilities is legal but useless to
		// the caller; fall back to the manifest's declaration so the UI can
		// still show what the plugin was installed for.
		capabilities = normaliseCapabilities(h.manifest.Capabilities)
		h.logger.Debug("Plugin reported no capabilities during handshake, using manifest",
			"plugin", h.manifest.ID)
	}
	logUnknownCapabilities(h.logger, h.manifest.ID, capabilities)
	return capabilities, nil
}

// normaliseCapabilities trims, drops blanks and removes duplicates while
// preserving the plugin's order.
func normaliseCapabilities(capabilities []domain.Capability) []domain.Capability {
	seen := make(map[domain.Capability]struct{}, len(capabilities))
	normalised := make([]domain.Capability, 0, len(capabilities))
	for _, capability := range capabilities {
		capability = domain.Capability(strings.TrimSpace(string(capability)))
		if capability == "" {
			continue
		}
		if _, duplicate := seen[capability]; duplicate {
			continue
		}
		seen[capability] = struct{}{}
		normalised = append(normalised, capability)
	}
	return normalised
}
