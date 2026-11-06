package lyrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"airmedy/internal/domain"
)

type LyricsService struct {
	repo         domain.LyricRepository
	logger       *slog.Logger
	providers    []domain.LyricsProvider
	localReader  domain.LocalLyricsReader
	settingsRepo domain.SettingsRepository
	tracks       domain.TrackRepository
	// pluginLyrics is the optional plugin-backed route. It is attached after
	// construction because plugin.Module is a sibling module and a nil is legal
	// (zero plugins must still boot).
	pluginLyrics domain.PluginLyricFetcher
}

// SetPluginLyricsFetcher installs the plugin-backed lyric route. A nil fetcher
// keeps the built-in providers only.
func (s *LyricsService) SetPluginLyricsFetcher(fetcher domain.PluginLyricFetcher) {
	s.pluginLyrics = fetcher
}

func NewLyricsService(repo domain.LyricRepository, logger *slog.Logger, providers []domain.LyricsProvider, localReader domain.LocalLyricsReader, settingsRepo domain.SettingsRepository, tracks domain.TrackRepository) *LyricsService {
	return &LyricsService{repo: repo, logger: logger, providers: providers, localReader: localReader, settingsRepo: settingsRepo, tracks: tracks}
}

func (s *LyricsService) GetLyrics(ctx context.Context, trackID string) (*domain.Lyric, error) {
	return s.repo.GetByTrackID(ctx, trackID)
}

func (s *LyricsService) SaveLyrics(ctx context.Context, trackID, content, source string) error {
	existing, _ := s.repo.GetByTrackID(ctx, trackID)
	if existing != nil {
		existing.Content = content
		existing.Source = source
		// A manually chosen lyric replaces the previous provider answer, so the
		// translation stored for that answer must not outlive it.
		existing.TranslationContent = ""
		existing.TranslationSource = ""
		return s.repo.Upsert(ctx, existing)
	}
	return s.repo.Upsert(ctx, &domain.Lyric{
		TrackID: trackID,
		Content: content,
		Source:  source,
	})
}

func (s *LyricsService) DeleteLyrics(ctx context.Context, trackID string) error {
	return s.repo.Delete(ctx, trackID)
}

// SaveLyricsFile writes content as a sibling .lrc file for audioPath. If a
// .lrc file already exists in any candidate location (dedicated lyrics
// folder, lyrics subfolder next to the track, or the track's own directory,
// searched in that priority order), it is updated in place. Otherwise a new
// file is created at the highest-priority enabled location.
func (s *LyricsService) SaveLyricsFile(ctx context.Context, audioPath, content string) error {
	if audioPath == "" {
		return errors.New("audio path required")
	}
	settings, err := s.settingsRepo.Load(ctx)
	if err != nil {
		settings = &domain.AppSettings{}
	}

	dir := filepath.Dir(audioPath)
	var candidates []string
	if settings.LyricsFolderEnabled && settings.LyricsFolderPath != "" {
		candidates = append(candidates, settings.LyricsFolderPath)
	}
	if settings.LyricsSubfolderEnabled && ValidSubfolderName(settings.LyricsSubfolderName) {
		candidates = append(candidates, ResolveSubdir(dir, settings.LyricsSubfolderName))
	}
	candidates = append(candidates, dir)

	base := strings.TrimSuffix(filepath.Base(audioPath), filepath.Ext(audioPath))

	for _, d := range candidates {
		path := filepath.Join(d, base+".lrc")
		if _, err := os.Stat(path); err == nil {
			return os.WriteFile(path, []byte(content), 0o644)
		}
	}

	targetDir := candidates[0]
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(targetDir, base+".lrc"), []byte(content), 0o644)
}

func (s *LyricsService) SearchLyrics(ctx context.Context, title, artist string, duration int, enableLrclib, enableKugou bool) ([]*domain.LyricsSearchResult, error) {
	var active []domain.LyricsProvider
	for _, p := range s.providers {
		if (p.Name() == "lrclib" && enableLrclib) || (p.Name() == "kugou" && enableKugou) {
			active = append(active, p)
		}
	}

	if len(active) == 0 {
		return nil, nil
	}

	type providerRes struct {
		results []*domain.LyricsSearchResult
		err     error
	}
	ch := make(chan providerRes, len(active))

	for _, p := range active {
		go func(p domain.LyricsProvider) {
			res, err := p.Search(ctx, title, artist, duration)
			ch <- providerRes{res, err}
		}(p)
	}

	var allResults []*domain.LyricsSearchResult
	for range len(active) {
		res := <-ch
		if res.err != nil {
			s.logger.Warn("lyrics provider search failed", "error", res.err)
			continue
		}
		allResults = append(allResults, res.results...)
	}

	return allResults, nil
}

// SaveMetaLyrics saves lyrics extracted from file metadata.
// Preserves any existing provider content/source fields.
func (s *LyricsService) SaveMetaLyrics(ctx context.Context, trackID, content, source string) error {
	existing, _ := s.repo.GetByTrackID(ctx, trackID)
	if existing != nil {
		existing.MetaContent = content
		existing.MetaSource = source
		return s.repo.Upsert(ctx, existing)
	}
	return s.repo.Upsert(ctx, &domain.Lyric{
		TrackID:     trackID,
		MetaContent: content,
		MetaSource:  source,
	})
}

// ResolveLyrics returns the best available lyric for display.
//
// "Local lyrics" tier = a sibling lyric file next to audioPath (read live, .lrc
// preferred over .txt) if present, otherwise the embedded metadata tag stored in
// the DB. When preferLocal is true, the local tier wins over external provider
// content; when false, provider content wins.
//
// Returns nil if nothing is available (caller may fetch from providers).
func (s *LyricsService) ResolveLyrics(ctx context.Context, trackID, audioPath string, preferLocal bool, extraDirs ...string) *domain.Lyric {
	resolved, _ := s.ResolveWithLocal(ctx, trackID, audioPath, preferLocal, extraDirs...)
	return resolved
}

// ResolveWithLocal resolves the best lyric (see ResolveLyrics) and additionally
// reports whether a local-tier lyric is present, derived from the SAME read.
//
// Callers that need both the display lyric and the "skip provider fetch"
// decision must use this instead of pairing ResolveLyrics with a separate
// HasLocalLyrics call: two independent reads double the chance that a transient
// DB error (e.g. SQLITE_BUSY during a concurrent sync) makes the presence check
// silently return false, wrongly triggering an online fetch.
func (s *LyricsService) ResolveWithLocal(ctx context.Context, trackID, audioPath string, preferLocal bool, extraDirs ...string) (resolved *domain.Lyric, hasLocal bool) {
	lyric, _ := s.repo.GetByTrackID(ctx, trackID)

	// Build the local tier: sibling file beats embedded metadata tag.
	var localContent, localSource string
	if s.localReader != nil {
		if content, source, found := s.localReader.Read(audioPath, extraDirs...); found {
			localContent, localSource = content, source
			s.logger.Debug("local lyric file found", "track_id", trackID, "path", audioPath, "source", source)
		} else {
			s.logger.Debug("no local lyric file", "track_id", trackID, "path", audioPath)
		}
	}
	if localContent == "" && lyric != nil && lyric.MetaContent != "" {
		localContent, localSource = lyric.MetaContent, lyric.MetaSource
		s.logger.Debug("using metadata tag lyrics", "track_id", trackID, "source", localSource)
	}

	var providerContent, providerSource, providerTranslationContent, providerTranslationSource string
	if lyric != nil && lyric.Content != "" {
		providerContent, providerSource = lyric.Content, lyric.Source
		providerTranslationContent, providerTranslationSource = lyric.TranslationContent, lyric.TranslationSource
	}

	local := func() *domain.Lyric {
		if localContent == "" {
			return nil
		}
		return &domain.Lyric{TrackID: trackID, Content: localContent, Source: localSource}
	}
	provider := func() *domain.Lyric {
		if providerContent == "" {
			return nil
		}
		return &domain.Lyric{
			TrackID:            trackID,
			Content:            providerContent,
			Source:             providerSource,
			TranslationContent: providerTranslationContent,
			TranslationSource:  providerTranslationSource,
		}
	}

	resolved = func() *domain.Lyric {
		if preferLocal {
			if l := local(); l != nil {
				return l
			}
			return provider()
		}
		if p := provider(); p != nil {
			return p
		}
		return local()
	}()

	hasLocal = localContent != ""

	if resolved != nil {
		s.logger.Debug("resolved lyrics", "track_id", trackID, "prefer_local", preferLocal, "source", resolved.Source)
	} else {
		s.logger.Debug("no lyrics resolved", "track_id", trackID, "prefer_local", preferLocal)
	}
	return resolved, hasLocal
}

// HasLocalLyrics reports whether a local-tier lyric exists for the track: a
// sibling lyric file next to audioPath, or an embedded metadata tag in the DB.
func (s *LyricsService) HasLocalLyrics(ctx context.Context, trackID, audioPath string, extraDirs ...string) bool {
	if s.localReader != nil {
		if _, _, found := s.localReader.Read(audioPath, extraDirs...); found {
			return true
		}
	}
	if lyric, _ := s.repo.GetByTrackID(ctx, trackID); lyric != nil && lyric.MetaContent != "" {
		return true
	}
	return false
}

// FetchFromProviders fetches lyrics from enabled providers concurrently.
// When both are enabled, the first non-nil result wins and the other request is cancelled.
func (s *LyricsService) FetchFromProviders(ctx context.Context, track *domain.TrackDTO, enableLrclib, enableKugou bool) (*domain.Lyric, error) {
	// A source plugin that produced this search row knows the platform's lyric
	// endpoint, so it gets the first attempt. A failed or non-timeline answer
	// falls through to the built-in providers rather than failing the lookup.
	if s.pluginLyrics != nil {
		if lyric, handled, err := s.pluginLyrics.FetchForTrack(ctx, track); handled {
			if lyric != nil {
				return s.saveLyric(ctx, lyric)
			}
			if err != nil {
				s.logger.Warn("plugin lyrics unavailable; falling back to built-in providers",
					"track_id", track.ID, "error", err)
			}
		}
	}

	var active []domain.LyricsProvider
	for _, p := range s.providers {
		if (p.Name() == "lrclib" && enableLrclib) || (p.Name() == "kugou" && enableKugou) {
			active = append(active, p)
		}
	}

	switch len(active) {
	case 0:
		return nil, nil
	case 1:
		l, err := active[0].Fetch(ctx, track)
		if err != nil || l == nil {
			return nil, err
		}
		return s.saveLyric(ctx, l)
	default:
		return s.fetchRace(ctx, track, active)
	}
}

func (s *LyricsService) fetchRace(ctx context.Context, track *domain.TrackDTO, providers []domain.LyricsProvider) (*domain.Lyric, error) {
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		lyric *domain.Lyric
		err   error
	}
	ch := make(chan result, len(providers))

	for _, p := range providers {
		go func(p domain.LyricsProvider) {
			l, e := p.Fetch(raceCtx, track)
			ch <- result{l, e}
		}(p)
	}

	var lastErr error
	for range len(providers) {
		r := <-ch
		if r.lyric != nil {
			cancel()
			return s.saveLyric(ctx, r.lyric)
		}
		if r.err != nil && !errors.Is(r.err, context.Canceled) {
			lastErr = r.err
		}
	}
	return nil, lastErr
}

// saveLyric persists a provider answer on top of the existing row. The
// translation travels with the original: saving only the content would make a
// cache hit or a restart drop the translated half.
func (s *LyricsService) saveLyric(ctx context.Context, incoming *domain.Lyric) (*domain.Lyric, error) {
	existing, _ := s.repo.GetByTrackID(ctx, incoming.TrackID)
	var lyric *domain.Lyric
	if existing != nil {
		existing.Content = incoming.Content
		existing.Source = incoming.Source
		existing.TranslationContent = incoming.TranslationContent
		existing.TranslationSource = incoming.TranslationSource
		lyric = existing
	} else {
		lyric = &domain.Lyric{
			TrackID:            incoming.TrackID,
			Content:            incoming.Content,
			Source:             incoming.Source,
			TranslationContent: incoming.TranslationContent,
			TranslationSource:  incoming.TranslationSource,
		}
	}

	if err := s.repo.Upsert(ctx, lyric); err != nil {
		s.logger.Warn("failed to save fetched lyrics", "track_id", incoming.TrackID, "error", err)
	}
	return lyric, nil
}

// ExportLyrics writes the stored original lyric as a sibling .lrc next to the
// audio file: <audio dir>/<audio base name>.lrc, UTF-8 with LF line endings.
// The audio file itself is never touched.
//
// Translations and word-by-word timing are deliberately not exported: merging
// them into one .lrc needs rules of its own.
//
// When the target exists and overwrite is false it returns
// domain.ErrLyricsFileExists together with the path, so the caller can offer
// overwrite / save as / cancel.
func (s *LyricsService) ExportLyrics(ctx context.Context, trackID string, overwrite bool) (string, error) {
	path, lyric, err := s.exportSource(ctx, trackID)
	if err != nil {
		return "", err
	}
	if !overwrite {
		if _, statErr := os.Stat(path); statErr == nil {
			return path, fmt.Errorf("%w: %s", domain.ErrLyricsFileExists, path)
		} else if !os.IsNotExist(statErr) {
			return "", fmt.Errorf("export lyrics: inspect %s: %w", path, statErr)
		}
	}
	if err := writeLyricsFile(path, lyric); err != nil {
		return "", err
	}
	s.logger.Info("Exported lyrics", "track_id", trackID, "path", path)
	return path, nil
}

// LyricsExportName is the default file name the "save as" dialog offers.
func (s *LyricsService) LyricsExportName(ctx context.Context, trackID string) (string, error) {
	path, _, err := s.exportSource(ctx, trackID)
	if err != nil {
		return "", err
	}
	return filepath.Base(path), nil
}

// ExportLyricsTo writes the same content to a destination the user picked
// (the "save as" branch of the conflict prompt). The destination directory must
// exist; a missing one is reported instead of created, because a typo in the
// picker should not litter the disk.
func (s *LyricsService) ExportLyricsTo(ctx context.Context, trackID, dest string) (string, error) {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return "", errors.New("export lyrics: destination path required")
	}
	_, lyric, err := s.exportSource(ctx, trackID)
	if err != nil {
		return "", err
	}
	if err := writeLyricsFile(dest, lyric); err != nil {
		return "", err
	}
	s.logger.Info("Exported lyrics", "track_id", trackID, "path", dest)
	return dest, nil
}

// exportSource resolves the track, its on-disk location and the lyric text to
// export. The stored provider content is preferred; a track whose only lyric
// came from a file tag falls back to the extracted metadata text, which is the
// same original lyric the UI displays.
func (s *LyricsService) exportSource(ctx context.Context, trackID string) (string, string, error) {
	if s.tracks == nil {
		return "", "", errors.New("export lyrics: no track repository is available")
	}
	track, err := s.tracks.GetByID(ctx, trackID)
	if err != nil {
		return "", "", fmt.Errorf("export lyrics: load track %s: %w", trackID, err)
	}
	if track == nil {
		return "", "", fmt.Errorf("export lyrics: track %s not found", trackID)
	}
	path := strings.TrimSpace(track.Path)
	if track.IsRemote() || path == "" {
		return "", "", fmt.Errorf("export lyrics: track %s has no local file to put the .lrc next to", trackID)
	}
	if !filepath.IsAbs(path) {
		return "", "", fmt.Errorf("export lyrics: %q is not an absolute path", path)
	}

	lyric, err := s.repo.GetByTrackID(ctx, trackID)
	if err != nil {
		return "", "", fmt.Errorf("export lyrics: load lyrics for %s: %w", trackID, err)
	}
	if lyric == nil {
		return "", "", fmt.Errorf("%w: track %s", domain.ErrLyricsUnavailable, trackID)
	}
	content := lyric.Content
	if strings.TrimSpace(content) == "" {
		content = lyric.MetaContent
	}
	if strings.TrimSpace(content) == "" {
		return "", "", fmt.Errorf("%w: track %s", domain.ErrLyricsUnavailable, trackID)
	}

	target := strings.TrimSuffix(path, filepath.Ext(path)) + ".lrc"
	return target, content, nil
}

// writeLyricsFile writes content with LF endings through a temporary file in
// the destination directory, then renames it into place. A failure on a read-only
// folder, a locked file or a full disk therefore never leaves a partial .lrc.
func writeLyricsFile(dest, content string) error {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".airmedy-lrc-*")
	if err != nil {
		return fmt.Errorf("export lyrics: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("export lyrics: write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("export lyrics: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("export lyrics: replace %s: %w", dest, err)
	}
	removeTemp = false
	return nil
}
