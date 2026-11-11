package download

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"airmedy/internal/domain"

	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
)

// The module must resolve every dependency it declares. app.Module supplies the
// real repositories and adapters; this test supplies stand-ins for exactly those
// and validates the graph, so a provider that starts expecting a value nobody
// supplies fails here instead of at application start.
func TestModuleResolvesWithThePortsAppModuleProvides(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	app := fx.New(
		fx.NopLogger,
		fx.Supply(logger),
		fx.Supply(Ports{
			Downloader: stubDownloader{},
			Validator:  stubValidator{},
			Properties: stubProperties{},
			Tagger:     stubTagger{},
			Artwork:    stubArtwork{},
		}),
		fx.Provide(
			func() domain.DownloadJobRepository { return stubJobs{} },
			func() domain.TrackRepository { return stubTracks{} },
			func() domain.DownloadMergeRepository { return stubMergeRepo{} },
			func() LocalFileRemover { return stubFileRemover{} },
			func() domain.TxManager { return stubTx{} },
			func() domain.StreamResolver { return stubResolver{} },
			func() SettingsReader { return stubSettingsReader{} },
			func() domain.MediaDurationProbe { return stubDurationProbe{} },
			// The lyric ports are optional capabilities, but fx still needs a
			// provider for them or the module cannot be assembled.
			func() LyricsFetcher { return nil },
			func() LyricsSettingsReader { return nil },
			func() LibraryImporter { return stubLibrary{} },
			func() PlaylistAdder { return stubPlaylist{} },
			func() CachedAudio { return stubCachedAudio{} },
			func() domain.PromotionTaskRepository { return stubPromotionTasks{} },
			func() domain.CoverIdentityAssets { return stubCoverAssetsPort{} },
		),
		Module,
		fx.Invoke(func(*Service) {}),
	)
	require.NoError(t, app.Err())
}

// --- stand-ins: just enough of each port for the graph to build --------------

type stubCachedAudio struct{}

func (stubCachedAudio) CachedFileInfo(context.Context, *domain.TrackDTO) (string, string, bool, bool, error) {
	return "", "", false, false, nil
}
func (stubCachedAudio) EnsureComplete(context.Context, *domain.TrackDTO) (<-chan error, error) {
	done := make(chan error)
	close(done)
	return done, nil
}
func (stubCachedAudio) MarkPendingPromote(context.Context, *domain.TrackDTO) error { return nil }
func (stubCachedAudio) DiscardEntry(context.Context, *domain.TrackDTO) error       { return nil }
func (stubCachedAudio) PendingPromotions(context.Context) ([]*domain.StreamCacheEntry, error) {
	return nil, nil
}
func (stubCachedAudio) PendingDiscards(context.Context) ([]*domain.StreamCacheEntry, error) {
	return nil, nil
}
func (stubCachedAudio) HasActiveSession(string, string) bool                      { return false }
func (stubCachedAudio) SetPromoted(context.Context, *domain.TrackDTO, bool) error { return nil }
func (stubCachedAudio) SetDiscardPending(context.Context, *domain.TrackDTO, bool) error {
	return nil
}
func (stubCachedAudio) ClearPendingPromote(context.Context, *domain.TrackDTO) error { return nil }

type stubPromotionTasks struct{ domain.PromotionTaskRepository }

type stubCoverAssetsPort struct {
	asset  string
	remote string
	data   []byte
	mime   string
}

func (s stubCoverAssetsPort) AssetForIdentity(context.Context, string, string) (string, error) {
	return s.asset, nil
}
func (s stubCoverAssetsPort) RemoteForIdentity(context.Context, string, string) (string, error) {
	return s.remote, nil
}
func (s stubCoverAssetsPort) BytesForIdentity(context.Context, string, string) ([]byte, string, error) {
	return s.data, s.mime, nil
}

type stubDownloader struct{}

func (stubDownloader) Download(context.Context, *domain.TrackDTO, domain.StreamRef, domain.DownloadTarget, domain.DownloadProgressFunc) (domain.DownloadResult, error) {
	return domain.DownloadResult{}, nil
}
func (stubDownloader) PartPath(dir, name string) string { return dir + "/" + name }
func (stubDownloader) TmpDir(dir string) string         { return dir + "/.tmp" }

type stubValidator struct{}

func (stubValidator) ValidateFile(string) (string, error) { return "mp3", nil }

type stubProperties struct{}

func (stubProperties) Duration(string) (time.Duration, error) { return time.Minute, nil }

type stubDurationProbe struct{}

func (stubDurationProbe) Probe(context.Context, string) (time.Duration, error) {
	return time.Minute, nil
}

type stubTagger struct{}

func (stubTagger) Tag(context.Context, string, domain.MetadataUpdate) error { return nil }
func (stubTagger) Read(context.Context, string) (domain.TrackMetadataResult, error) {
	return domain.TrackMetadataResult{}, nil
}

type stubArtwork struct{}

func (stubArtwork) Fetch(context.Context, string) ([]byte, string, error) { return nil, "", nil }

type stubJobs struct{ domain.DownloadJobRepository }

type stubTracks struct{ domain.TrackRepository }

type stubMergeRepo struct{ domain.DownloadMergeRepository }

type stubFileRemover struct{}

func (stubFileRemover) DeleteTrackFile(context.Context, string) error { return nil }

type stubTx struct{ domain.TxManager }

type stubResolver struct{}

func (stubResolver) Resolve(context.Context, *domain.TrackDTO, string) (domain.StreamRef, error) {
	return domain.StreamRef{URL: "http://example/x.mp3"}, nil
}

type stubSettingsReader struct{}

func (stubSettingsReader) GetSettings(context.Context) (*domain.AppSettings, error) {
	return &domain.AppSettings{}, nil
}

type stubLibrary struct{}

func (stubLibrary) ImportFileWithID(context.Context, string) (string, error) { return "t", nil }
func (stubLibrary) VerifyImportedTrack(context.Context, string) error        { return nil }

type stubPlaylist struct{}

func (stubPlaylist) AddTrack(context.Context, string, string) error { return nil }
