package downloader

import (
	"context"

	"airmedy/internal/domain"
)

// Compile-time proof that the downloader satisfies the port the app layer
// depends on (domain.StreamDownloader).
var _ domain.StreamDownloader = (*Downloader)(nil)

// Download implements domain.StreamDownloader. The domain value types are
// translated here so the app layer never sees the infra ones.
func (d *Downloader) Download(ctx context.Context, track *domain.TrackDTO, ref domain.StreamRef, target domain.DownloadTarget, progress domain.DownloadProgressFunc) (domain.DownloadResult, error) {
	res, err := d.DownloadInto(ctx, track, ref, Target{
		Dir:              target.Dir,
		Name:             target.Name,
		Quality:          target.Quality,
		ExpectedDuration: target.ExpectedDuration,
	}, ProgressFunc(progress))
	if err != nil {
		return domain.DownloadResult{}, err
	}
	return domain.DownloadResult{
		Path:      res.Path,
		Container: string(res.Container),
		Bytes:     res.Bytes,
	}, nil
}

// ValidateFile implements domain.FileValidator. It mirrors the package-level
// ValidateFile; the method exists so a Downloader value can be handed to the app
// layer as the port it needs.
func (d *Downloader) ValidateFile(path string) (string, error) {
	c, err := ValidateFile(path)
	if err != nil {
		return "", err
	}
	return string(c), nil
}
