package main

import (
	"io"
	"log/slog"
	"testing"
	"testing/fstest"

	"airmedy/internal/app/config"
	applyrics "airmedy/internal/app/lyrics"
	"airmedy/internal/domain"
	lyricsinfra "airmedy/internal/infra/lyrics"

	"github.com/natefinch/lumberjack"
	"go.uber.org/fx"
)

// TestFxGraphIsSatisfiable assembles the real provider set without starting the
// app. A port that nothing provides fails here instead of on launch — the class
// of bug that shipped twice (domain.StreamResolver, download.SettingsReader)
// because every unit test supplies its own stub.
func TestFxGraphIsSatisfiable(t *testing.T) {
	deps := fxDeps{
		Config:     &config.Config{DataDir: t.TempDir()},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		LogRotator: &lumberjack.Logger{Filename: t.TempDir() + "/test.log"},
		RemoteFS:   fstest.MapFS{},
	}

	// ValidateApp builds the dependency graph without invoking providers, so it
	// stays side-effect free even though the graph contains real adapters.
	if err := fx.ValidateApp(fxOptions(deps)); err != nil {
		t.Fatalf("fx graph is not satisfiable: %v", err)
	}
}

// TestBuiltInLyricsProvidersExistWithoutPlugins pins the boundary the plugin
// work must not blur: lyrics are a core feature. The graph assembled here has
// the lyrics modules and no plugin module at all, yet the lyrics service still
// receives the built-in LRCLIB and Kugou providers plus the local .lrc reader.
//
// Invokes run inside fx.New, before any OnStart hook, so this constructs the
// lyrics chain without starting the app (no database or audio device needed).
func TestBuiltInLyricsProvidersExistWithoutPlugins(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var providers []domain.LyricsProvider
	var service *applyrics.LyricsService

	app := fx.New(
		fx.NopLogger,
		fx.Supply(logger),
		lyricsinfra.Module,
		applyrics.Module,
		// A lyrics store, settings and track lookup are not needed to prove
		// provider wiring; nil stubs keep the test free of SQLite.
		fx.Provide(func() domain.LyricRepository { return nil }),
		fx.Provide(func() domain.SettingsRepository { return nil }),
		fx.Provide(func() domain.TrackRepository { return nil }),
		fx.Populate(&service),
		fx.Invoke(fx.Annotate(
			func(list []domain.LyricsProvider) { providers = list },
			fx.ParamTags(`group:"lyrics_providers"`),
		)),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("lyrics graph without plugins: %v", err)
	}
	if service == nil {
		t.Fatal("the lyrics service was not constructed")
	}

	var lrclib, kugou bool
	for _, provider := range providers {
		switch provider.(type) {
		case *lyricsinfra.LrclibProvider:
			lrclib = true
		case *lyricsinfra.KugouProvider:
			kugou = true
		}
	}
	if !lrclib || !kugou {
		t.Fatalf("built-in lyrics providers missing: lrclib=%v kugou=%v (all: %T)", lrclib, kugou, providers)
	}
}
