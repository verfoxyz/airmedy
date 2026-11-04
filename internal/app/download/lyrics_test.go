package download

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"airmedy/internal/domain"
)

// fakeLyricsFetcher records the provider flags it was called with, so the test
// can assert the settings → provider policy rather than just "it was called".
type fakeLyricsFetcher struct {
	lyric  *domain.Lyric
	err    error
	lrclib bool
	kugou  bool
	calls  int
}

func (f *fakeLyricsFetcher) FetchFromProviders(
	_ context.Context, _ *domain.TrackDTO, enableLrclib, enableKugou bool,
) (*domain.Lyric, error) {
	f.calls++
	f.lrclib, f.kugou = enableLrclib, enableKugou
	if f.err != nil {
		return nil, f.err
	}
	return f.lyric, nil
}

type fakeLyricsSettings struct {
	settings *domain.AppSettings
	err      error
}

func (f fakeLyricsSettings) GetSettings(context.Context) (*domain.AppSettings, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.settings, nil
}

func quietLyricsLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func lyricTrack() *domain.TrackDTO {
	return &domain.TrackDTO{Track: domain.Track{ID: "t1", Title: "晴天"}}
}

// TestDownloadLyrics_HonoursTheProviderToggles: the download setting says "fetch
// lyrics", the provider toggles say where from. Both must reach the fetcher.
func TestDownloadLyrics_HonoursTheProviderToggles(t *testing.T) {
	cases := map[string]struct {
		settings   *domain.AppSettings
		wantLrclib bool
		wantKugou  bool
		wantCalls  int
	}{
		"both enabled by default": {
			settings:   &domain.AppSettings{EnableLrclib: true, EnableKugou: true},
			wantLrclib: true, wantKugou: true, wantCalls: 1,
		},
		"lrclib only": {
			settings:   &domain.AppSettings{EnableLrclib: true},
			wantLrclib: true, wantKugou: false, wantCalls: 1,
		},
		"kugou only": {
			settings:   &domain.AppSettings{EnableKugou: true},
			wantLrclib: false, wantKugou: true, wantCalls: 1,
		},
		"neither enabled means no request at all": {
			settings:   &domain.AppSettings{},
			wantLrclib: false, wantKugou: false, wantCalls: 0,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fetcher := &fakeLyricsFetcher{lyric: &domain.Lyric{Content: "[00:00]歌词", Source: "lrclib"}}
			adapter := NewDownloadLyrics(fetcher, fakeLyricsSettings{settings: tc.settings}, quietLyricsLog())

			if err := adapter.FetchForTrack(context.Background(), lyricTrack()); err != nil {
				t.Fatalf("FetchForTrack: %v", err)
			}
			if fetcher.calls != tc.wantCalls {
				t.Fatalf("fetcher called %d times, want %d", fetcher.calls, tc.wantCalls)
			}
			if tc.wantCalls > 0 && (fetcher.lrclib != tc.wantLrclib || fetcher.kugou != tc.wantKugou) {
				t.Fatalf("providers = (lrclib=%v, kugou=%v), want (%v, %v)",
					fetcher.lrclib, fetcher.kugou, tc.wantLrclib, tc.wantKugou)
			}
		})
	}
}

// TestDownloadLyrics_NoLyricsIsNotAFailure: a track without lyrics is normal.
// Returning an error would fail an otherwise successful download.
func TestDownloadLyrics_NoLyricsIsNotAFailure(t *testing.T) {
	adapter := NewDownloadLyrics(
		&fakeLyricsFetcher{lyric: nil},
		fakeLyricsSettings{settings: &domain.AppSettings{EnableLrclib: true}},
		quietLyricsLog(),
	)
	if err := adapter.FetchForTrack(context.Background(), lyricTrack()); err != nil {
		t.Fatalf("FetchForTrack with no lyrics = %v, want nil", err)
	}

	adapter = NewDownloadLyrics(
		&fakeLyricsFetcher{lyric: &domain.Lyric{Content: "   "}},
		fakeLyricsSettings{settings: &domain.AppSettings{EnableLrclib: true}},
		quietLyricsLog(),
	)
	if err := adapter.FetchForTrack(context.Background(), lyricTrack()); err != nil {
		t.Fatalf("FetchForTrack with blank lyrics = %v, want nil", err)
	}
}

// TestDownloadLyrics_ProviderFailureIsReported: a genuine fetch failure must
// surface so the finalize step can log it; it stays non-fatal there.
func TestDownloadLyrics_ProviderFailureIsReported(t *testing.T) {
	adapter := NewDownloadLyrics(
		&fakeLyricsFetcher{err: errors.New("upstream 502")},
		fakeLyricsSettings{settings: &domain.AppSettings{EnableLrclib: true}},
		quietLyricsLog(),
	)
	if err := adapter.FetchForTrack(context.Background(), lyricTrack()); err == nil {
		t.Fatal("FetchForTrack = nil, want the provider failure")
	}
}

// A missing lyrics capability must degrade to a no-op, never to a panic: the
// finalizer is wired whether or not a deployment has lyrics.
func TestDownloadLyrics_MissingFetcherIsANoOp(t *testing.T) {
	adapter := NewDownloadLyrics(nil, nil, quietLyricsLog())
	if err := adapter.FetchForTrack(context.Background(), lyricTrack()); err != nil {
		t.Fatalf("FetchForTrack without a fetcher = %v, want nil", err)
	}
}
