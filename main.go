package main

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"airmedy/internal/app"
	"airmedy/internal/app/appsettings"
	appcatalog "airmedy/internal/app/catalog"
	"airmedy/internal/app/config"
	"airmedy/internal/app/download"
	"airmedy/internal/app/i18n"
	"airmedy/internal/app/lx"
	"airmedy/internal/app/lyrics"
	"airmedy/internal/app/plugin"
	"airmedy/internal/app/pluginkv"
	"airmedy/internal/app/remoteserver"
	"airmedy/internal/app/singleinstance"
	appstream "airmedy/internal/app/stream"
	"airmedy/internal/domain"
	"airmedy/internal/infra/artwork"
	"airmedy/internal/infra/downloader"
	"airmedy/internal/infra/logging"
	"airmedy/internal/infra/metadata"
	"airmedy/internal/infra/pluginhost"
	"airmedy/internal/infra/sysinfo"
	"airmedy/internal/infra/wails"
	"runtime/debug"

	"github.com/natefinch/lumberjack"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/icons"
	"go.uber.org/fx"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed remote/dist
var remoteAssets embed.FS

//go:embed assets/mac-tray-icon.png
var macTrayIcon []byte

//go:embed assets/linux-tray-icon.png
var linuxTrayIcon []byte

//go:embed assets/windows-tray-icon.png
var windowsTrayIcon []byte

func init() {
	application.RegisterEvent[string]("time")
	application.RegisterEvent[string]("language:changed")
}

const singleInstanceID = "me.misa198.airmedy"

// fxDeps carries the values main constructs before the container exists.
type fxDeps struct {
	Config     *config.Config
	Logger     *slog.Logger
	LogRotator *lumberjack.Logger
	RemoteFS   fs.FS
}

// fxOptions builds the container's provider set.
//
// It is a function rather than an inline literal so a test can run
// fx.ValidateApp over the real graph. A port with no provider only fails when
// the app starts, and that has already reached this file twice (StreamResolver,
// then download.SettingsReader); the test catches it in milliseconds instead.
func fxOptions(deps fxDeps) fx.Option {
	return fx.Options(

		app.Module,
		lx.Module,
		plugin.Module,
		appstream.Module,
		// Supply the logger built before fx so the logging module wires rotation
		// onto the same instance instead of constructing its own.
		fx.Supply(deps.LogRotator, deps.Logger),
		// Plugin host: the registry lives in app/plugin and may not import infra,
		// so the manifest loader and the process-backed host factory are bound
		// onto domain ports here, next to the download adapters.
		fx.Provide(func() lx.Config {
			return lx.Config{SourcesDir: deps.Config.LxSourcesDir()}
		}),
		// The lx source list reaches the plugin through its launch environment.
		// The wiring lives here because the registry must stay free of an app/lx
		// dependency; the registry itself implements no lx semantics, it just
		// asks the hook for KEY=VALUE pairs at every start.
		fx.Provide(func(sources *lx.Service) plugin.Config {
			config := plugin.Config{Root: deps.Config.PluginsDir()}
			config.LaunchEnv = func(ctx context.Context, pluginID string) []string {
				if pluginID != lx.PluginID {
					return nil
				}
				env, err := sources.LaunchEnv(ctx)
				if err != nil {
					deps.Logger.Warn("Failed to build the lx launch environment", "error", err)
					return nil
				}
				return env
			}
			return config
		}),
		// The two services need each other at run time (the source list needs a
		// restart, the registry needs a launch environment), so the edge is
		// attached after construction instead of injected: fx rejects a cycle
		// even when only the call path is circular.
		fx.Invoke(func(sources *lx.Service, registry *plugin.Service) {
			sources.AttachRegistry(registry)
		}),
		fx.Provide(func(logger *slog.Logger) domain.PluginLoader {
			return pluginhost.NewLoader(logger)
		}),
		// The plugin -> host request set (storage.* today): the handler is bound
		// on the host factory, which every launch goes through.
		fx.Provide(func(storage domain.PluginKVRepository, logger *slog.Logger) *pluginkv.Handler {
			return pluginkv.NewHandler(storage, nil, logger)
		}),
		// `permissions.kv` is enforced per plugin, so the handler needs the
		// registry's manifest table. The edge is attached after construction
		// because the registry consumes the factory that carries the handler: a
		// constructor argument would close an fx cycle (the same reason app/lx
		// gets its registry attached this way).
		fx.Invoke(func(handler *pluginkv.Handler, registry *plugin.Service) {
			handler.AttachPermission(registry.PermitsKV)
		}),
		fx.Provide(func(logger *slog.Logger, handler *pluginkv.Handler) domain.PluginHostFactory {
			return pluginhost.NewFactory(logger, pluginhost.WithRequestHandler(handler.Handle)).Start
		}),
		// Stream resolution is plugin-only: a provider is playable only when an
		// enabled plugin declares it under "providers" and implements
		// stream.resolve. app/stream's resolver is the single
		// domain.StreamResolver the download service consumes (bound by
		// appstream.Module above).
		fx.Provide(func(service *plugin.Service) appstream.ProviderRegistry {
			return service
		}),
		// Catalog search asks the resolver "could this provider be downloaded?"
		// so a source that can search but has no resolver is disabled up front
		// instead of failing after the click. The concrete resolver is bound onto
		// catalog's narrow availability port here, which keeps app/catalog free
		// of an app/stream import.
		fx.Provide(func(resolver *appstream.PluginResolver) appcatalog.ResolverAvailability {
			return resolver
		}),
		// The source table (which plugins can search what) and the per-source
		// call both go through the plugin registry; the port keeps app/catalog
		// free of an app/plugin import.
		fx.Provide(func(service *plugin.Service) appcatalog.PluginRegistry {
			return service
		}),
		// The search call reads the plugin's "show covers" switch from the
		// host-owned setting store. Binding it here keeps app/catalog free of an
		// app/plugin import.
		fx.Provide(func(store *plugin.SettingStore) appcatalog.PluginSettingReader {
			return store
		}),
		fx.Provide(func() remoteserver.RemoteFS { return remoteserver.RemoteFS{FS: deps.RemoteFS} }),
		// The download pipeline's two lyric ports: the lyrics service and the
		// settings service are concrete app types from other modules, so main
		// joins them onto download's narrow interfaces here rather than letting
		// app/download import either package.
		fx.Provide(func(svc *lyrics.LyricsService) download.LyricsFetcher { return svc }),
		fx.Provide(func(svc *appsettings.SettingsService) download.LyricsSettingsReader { return svc }),
		// Download pipeline adapters: the ports live in domain, the concrete
		// values belong to infra, and only main.go may join the two (app must
		// not import infra). download.Module builds the service from them.
		fx.Provide(func(tagger *metadata.Tagger, logger *slog.Logger) download.Ports {
			return download.BindPorts(
				downloader.New(tagger),
				downloader.New(tagger),
				tagger,
				tagger,
				artwork.NewCoverFetcher(),
			)
		}),
		fx.Provide(metadata.NewTagger),
		fx.NopLogger, // Keep logs clean for now
	)
}

func main() {
	debug.SetGCPercent(50)

	// Build the file logger before anything else (and before the fx graph) so
	// bootstrap logs and any startup failure are written to disk. On Windows GUI
	// builds there is no stderr, so the default slog logger is a black hole; if
	// an fx provider fails (e.g. an arch-specific CGO lib on Windows/arm64) we
	// would otherwise os.Exit(1) with no log file and no visible error.
	bootCfg, err := config.NewConfig()
	if err != nil {
		os.Exit(1)
	}
	logRotator, logger, err := logging.NewFileLogger(bootCfg)
	if err != nil {
		os.Exit(1)
	}
	defer func() { _ = logRotator.Close() }()

	sysinfo.RaiseFileDescriptorLimit(logger)

	// Single-instance guard must run before any exclusive resource is acquired
	// (the bleve index lock, the remote-server port). A second process forwards
	// its args — including the deep-link URL on Windows/Linux — and exits, rather
	// than booting a full instance that would deadlock on those resources.
	siInstance, err := singleinstance.Acquire(singleinstance.PortForID(singleInstanceID), os.Args)
	if err != nil {
		if err == singleinstance.ErrAlreadyRunning {
			slog.Info("another instance is running; forwarded args and exiting")
			return
		}
		slog.Warn("single instance guard unavailable, continuing", "error", err)
	}
	if siInstance != nil {
		defer func() { _ = siInstance.Close() }()
	}

	if err := registerProtocol(); err != nil {
		slog.Warn("failed to register deep link protocol", "error", err)
	}

	var greetService *wails.GreetService
	var libraryService *wails.LibraryService
	var playerService *wails.PlayerService
	var searchService *wails.SearchService
	var playlistService *wails.PlaylistService
	var lyricsService *wails.LyricsService
	var eqService *wails.EQService
	var normalizationService *wails.NormalizationService
	var analysisService *wails.AnalysisService
	var analyticsService *wails.AnalyticsService
	var windowService *wails.WindowService
	var i18nService *i18n.Service
	var settingsService *wails.SettingsService
	var updaterService *wails.UpdaterService
	var artworkCache domain.ArtworkCache
	var remoteServerService *wails.RemoteServerService
	var mobilePairingService *wails.MobilePairingService
	var mobileLibrarySyncService *wails.MobileLibrarySyncService
	var moodRadioService *wails.MoodRadioService
	var downloadService *wails.DownloadService
	var pluginService *wails.PluginService
	var lxSourcesService *wails.LxSourcesService
	var catalogService *wails.CatalogService
	var trackTransitionNotificationActivator domain.TrackTransitionNotificationActivator
	var (
		lastfmService *wails.LastFmService
		wailsApp      *application.App
	)

	slog.Info("Starting Airmedy", "version", config.Version)

	remoteFS, _ := fs.Sub(remoteAssets, "remote/dist")

	fxApp := fx.New(
		fxOptions(fxDeps{
			Config:     bootCfg,
			Logger:     logger,
			LogRotator: logRotator,
			RemoteFS:   remoteFS,
		}),
		fx.Populate(&greetService, &libraryService, &playerService, &searchService, &playlistService, &lyricsService, &eqService, &normalizationService, &analysisService, &analyticsService, &windowService, &i18nService, &settingsService, &lastfmService, &updaterService, &artworkCache, &remoteServerService, &mobilePairingService, &mobileLibrarySyncService, &moodRadioService, &trackTransitionNotificationActivator, &downloadService, &pluginService, &lxSourcesService, &catalogService),
		fx.NopLogger, // Keep logs clean for now
	)

	startCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := fxApp.Start(startCtx); err != nil {
		slog.Error("failed to start services", "error", err)
		os.Exit(1)
	}

	var stopOnce sync.Once
	stopFX := func() {
		stopOnce.Do(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := fxApp.Stop(stopCtx); err != nil {
				slog.Error("error stopping services", "error", err)
			}
		})
	}

	var mainWindow *application.WebviewWindow

	wailsApp = application.New(application.Options{
		Name:        "airmedy",
		Description: "A modern music player",
		// Wails invokes OnShutdown synchronously from macOS'
		// applicationShouldTerminate callback. Unlike ApplicationWillTerminate
		// event listeners, this blocks Cmd+Q until the FX lifecycle has persisted
		// the current playback state.
		OnShutdown: stopFX,
		Services: []application.Service{
			application.NewService(greetService),
			application.NewService(libraryService),
			application.NewService(playerService),
			application.NewService(searchService),
			application.NewService(playlistService),
			application.NewService(lyricsService),
			application.NewService(eqService),
			application.NewService(normalizationService),
			application.NewService(analysisService),
			application.NewService(analyticsService),
			application.NewService(lastfmService),
			application.NewService(windowService),
			application.NewService(settingsService),
			application.NewService(updaterService),
			application.NewService(remoteServerService),
			application.NewService(mobilePairingService),
			application.NewService(mobileLibrarySyncService),
			application.NewService(moodRadioService),
			application.NewService(downloadService),
			application.NewService(pluginService),
			application.NewService(lxSourcesService),
			application.NewService(catalogService),
		},
		Assets: application.AssetOptions{
			Handler: wails.NewAssetHandler(assets, artworkCache),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		Linux: application.LinuxOptions{
			// X11 fallback: sets g_set_prgname so the window's WM_CLASS is "airmedy",
			// matching StartupWMClass in org.wails.airmedy.desktop. On Wayland/modern
			// GNOME the dock icon is matched via the GApplication app_id
			// ("org.wails.airmedy"), which is why the .desktop file is named to match.
			ProgramName: "airmedy",
		},
	})
	lastfmService.GetService().RefreshUserAvatar()

	// Initialize i18n
	settings, _ := settingsService.GetSettings(context.Background())
	i18nService.SetLocale(settings.Language)

	// Create application menu
	menu := wails.BuildAppMenu(wailsApp, i18nService, playerService)
	wailsApp.Menu.SetApplicationMenu(menu)

	mainWindow = wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:              "Airmedy",
		Width:              1280,
		Height:             800,
		MinWidth:           1200,
		MinHeight:          768,
		UseApplicationMenu: runtime.GOOS == "darwin",
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 50,
			Backdrop:                application.MacBackdropTranslucent,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		Windows: application.WindowsWindow{
			Theme:       winTheme(settings.Theme),
			CustomTheme: winCustomTheme(settings.Theme),
		},
		BackgroundColour: bgRGBA(settings.Theme),
		URL:              "/",
	})

	mainWindow.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		mainWindow.Hide()
		e.Cancel()
	})
	windowService.SetMainWindow(mainWindow)
	trackTransitionNotificationActivator.SetTrackTransitionActivationCallback(func() {
		application.InvokeAsync(windowService.ShowMain)
	})
	windowService.SetTitleBarTheme(settings.Theme)
	mainWindow.Show()
	mainWindow.Focus()

	thumbBarMgr := wails.NewThumbBarManager(playerService.GetService())
	thumbBarMgr.Setup(mainWindow)

	// Wire the SMTC "Now Playing" card click to show the correct window.
	// windowService.ShowCurrent() focuses the mini player if open, otherwise
	// the main window — matching the tray "Show Airmedy" behaviour.
	playerService.GetService().SetNowPlayingActivateCallback(func() {
		application.InvokeAsync(func() {
			windowService.ShowCurrent()
		})
	})

	// Wails/AppKit can reset a floating window's level while macOS reactivates
	// the app from its Dock icon. Reapply the persisted mini-player pin state
	// after both activation and reopen, without changing the user's preference.
	if runtime.GOOS == "darwin" {
		restoreMiniPin := func(_ *application.ApplicationEvent) {
			application.InvokeAsync(windowService.RestoreMiniAlwaysOnTop)
		}
		wailsApp.Event.OnApplicationEvent(events.Mac.ApplicationDidBecomeActive, restoreMiniPin)
		wailsApp.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, restoreMiniPin)
	}

	windowService.SetMiniWindowFactory(func() *application.WebviewWindow {
		w := wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
			Title:               "Mini Player",
			Width:               300,
			Height:              300,
			MinWidth:            280,
			MinHeight:           280,
			MaxWidth:            500,
			MaxHeight:           1000,
			Hidden:              true,
			AlwaysOnTop:         false,
			DisableResize:       false,
			Frameless:           runtime.GOOS == "windows",
			MinimiseButtonState: application.ButtonHidden,
			MaximiseButtonState: application.ButtonHidden,
			CloseButtonState:    application.ButtonHidden,
			Mac: application.MacWindow{
				InvisibleTitleBarHeight: 0,
				Backdrop:                application.MacBackdropTranslucent,
				TitleBar:                application.MacTitleBarHiddenInset,
				CollectionBehavior:      application.MacWindowCollectionBehaviorTransient,
			},
			Windows: application.WindowsWindow{
				Theme:       winTheme(settings.Theme),
				CustomTheme: winCustomTheme(settings.Theme),
			},
			BackgroundColour: bgRGBA(settings.Theme),
			URL:              "/?mode=mini#/mini-player",
		})
		wails.LockMiniPlayerAspect(w, false)
		// Hidden GTK windows have no X11 surface until shown, so retry once the
		// native surface exists. The macOS/Windows helpers are idempotent.
		w.RegisterHook(events.Common.WindowShow, func(_ *application.WindowEvent) {
			wails.LockMiniPlayerAspect(w, false)
		})
		// Restore saved geometry + pin mode, clamped to the current screen layout.
		windowService.ApplyMiniState(w)
		// Persist geometry as the user moves/resizes the window (debounced).
		w.RegisterHook(events.Common.WindowDidMove, func(_ *application.WindowEvent) {
			windowService.SaveMiniGeometry()
		})
		w.RegisterHook(events.Common.WindowDidResize, func(_ *application.WindowEvent) {
			windowService.SaveMiniGeometry()
		})
		w.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
			windowService.SaveMiniGeometry()
			windowService.OnMiniPlayerClosed()
			// No e.Cancel() — Wails destroys the window, freeing its memory
		})
		return w
	})

	// Handle deep links (e.g. airmedy://auth?token=...). Shared by the macOS
	// Apple-Event path and the Windows/Linux second-instance relay.
	handleDeepLink := func(urlStr string) {
		if urlStr == "" {
			return
		}
		u, err := url.Parse(urlStr)
		if err != nil {
			return
		}
		if u.Scheme == "airmedy" && u.Host == "auth" {
			token := u.Query().Get("token")
			if token != "" {
				go func() {
					if err := lastfmService.GetService().CompleteAuth(context.Background(), token); err != nil {
						slog.Error("failed to complete Last.fm auth", "error", err)
					} else {
						slog.Info("Last.fm auth completed successfully")
						if wailsApp != nil {
							wailsApp.Event.Emit("lastfm:connected")
						}
					}
				}()
			}
		}
	}

	// macOS: URL-scheme launches arrive as an Apple Event on the running instance.
	wailsApp.Event.OnApplicationEvent(events.Common.ApplicationLaunchedWithUrl, func(e *application.ApplicationEvent) {
		handleDeepLink(e.Context().URL())
	})

	// Windows/Linux: a second process relays its args (incl. the deep-link URL).
	if siInstance != nil {
		go func() {
			for args := range siInstance.Messages() {
				if mainWindow != nil {
					mainWindow.Show()
					mainWindow.Focus()
				}
				for _, a := range args {
					if strings.HasPrefix(a, "airmedy://") {
						handleDeepLink(a)
					}
				}
			}
		}()
	}

	var trayManager *wails.TrayManager
	settings, err = settingsService.GetSettings(context.Background())
	if err == nil && settings.ShowTrayIcon {
		systemTray := wailsApp.SystemTray.New()
		switch runtime.GOOS {
		case "darwin":
			systemTray.SetTemplateIcon(icons.SystrayMacTemplate)
			systemTray.SetIcon(macTrayIcon)
		case "linux":
			systemTray.SetIcon(linuxTrayIcon)
		case "windows":
			systemTray.SetIcon(windowsTrayIcon)
		}
		systemTray.SetTooltip("Airmedy")

		trayManager = wails.NewTrayManager(wailsApp, playerService.GetService(), libraryService, i18nService, windowService)
		trayManager.Setup(systemTray, mainWindow)
	}

	// Listen for language changes
	wailsApp.Event.On("language:changed", func(event *application.CustomEvent) {
		if lang, ok := event.Data.(string); ok {
			i18nService.SetLocale(lang)
			application.InvokeSync(func() {
				newMenu := wails.BuildAppMenu(wailsApp, i18nService, playerService)
				wailsApp.Menu.SetApplicationMenu(newMenu)
				if trayManager != nil {
					trayManager.UpdateLanguage()
				}
			})
		}
	})

	go func() {
		for {
			now := time.Now().Format(time.RFC1123)
			wailsApp.Event.Emit("time", now)
			time.Sleep(time.Second)
		}
	}()

	if err := wailsApp.Run(); err != nil {
		slog.Error("application error", "error", err)
		os.Exit(1)
	}

	thumbBarMgr.Stop()
	stopFX()
}

// bgRGBA returns the BackgroundColour matching the app theme's --bg-main value.
func bgRGBA(appTheme string) application.RGBA {
	switch appTheme {
	case "light":
		return application.NewRGB(244, 244, 245) // #f4f4f5
	case "black":
		return application.NewRGB(10, 10, 10) // #0a0a0a
	default: // "dark", "system"
		return application.NewRGB(24, 24, 27) // #18181b
	}
}

// winTheme returns the Wails Windows Theme constant for the given app theme.
func winTheme(appTheme string) application.Theme {
	switch appTheme {
	case "dark", "black":
		return application.Dark
	case "light":
		return application.Light
	default: // "system"
		return application.SystemDefault
	}
}

// winCustomTheme builds a ThemeSettings whose title bar colours match --bg-main.
// Colors are uint32 in 0x00BBGGRR format (DWM / Wails convention).
func winCustomTheme(appTheme string) application.ThemeSettings {
	ptr := func(v uint32) *uint32 { return &v }

	// dark / black palette
	darkBar := ptr(0x001B1818)  // #18181b
	darkText := ptr(0x00FFFFFF) // white
	if appTheme == "black" {
		darkBar = ptr(0x000A0A0A) // #0a0a0a
	}
	darkTheme := &application.WindowTheme{
		TitleBarColour:  darkBar,
		TitleTextColour: darkText,
		BorderColour:    darkBar,
	}

	// light palette
	lightBar := ptr(0x00F5F4F4)    // #f4f4f5
	lightText := ptr(0x000A0A0A)   // near-black
	lightBorder := ptr(0x00E5E4E4) // subtle border, slightly darker than bg
	lightTheme := &application.WindowTheme{
		TitleBarColour:  lightBar,
		TitleTextColour: lightText,
		BorderColour:    lightBorder,
	}

	return application.ThemeSettings{
		DarkModeActive:    darkTheme,
		DarkModeInactive:  darkTheme,
		LightModeActive:   lightTheme,
		LightModeInactive: lightTheme,
	}
}
