package domain

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestClassifyDownloadError_RateLimitBacksOffLonger(t *testing.T) {
	p := ClassifyDownloadError(fmt.Errorf("%w: http 429", ErrStreamRateLimited), 0, DefaultDownloadMaxAttempts)
	assert.True(t, p.Retryable)
	assert.Equal(t, RateLimitRetryDelay, p.Delay)

	// A second 429 waits twice as long: retrying a rate limit quickly is what
	// gets an account blocked.
	second := ClassifyDownloadError(ErrStreamRateLimited, 1, DefaultDownloadMaxAttempts)
	assert.True(t, second.Retryable)
	assert.Equal(t, 2*RateLimitRetryDelay, second.Delay)
}

func TestClassifyDownloadError_PermanentErrorsNeverRetry(t *testing.T) {
	for _, err := range []error{
		ErrStreamUnavailable, // VIP / -110 / no url
		ErrStreamAuth,        // 403
		ErrDownloadPermanent, // 404 / 410 / bad scheme
		ErrCorruptDownload,   // validated and rejected: same URL will not help
	} {
		t.Run(err.Error(), func(t *testing.T) {
			p := ClassifyDownloadError(err, 0, DefaultDownloadMaxAttempts)
			assert.False(t, p.Retryable)
			assert.Zero(t, p.Delay)
		})
	}
}

func TestClassifyDownloadError_TransportErrorsRetryWithEscalatingBackoff(t *testing.T) {
	// A bare error is a transport failure (timeout, reset, truncation).
	transport := errors.New("download: request failed: connection reset")

	first := ClassifyDownloadError(transport, 0, DefaultDownloadMaxAttempts)
	assert.True(t, first.Retryable)
	assert.Equal(t, 5*time.Second, first.Delay)

	second := ClassifyDownloadError(transport, 1, DefaultDownloadMaxAttempts)
	assert.True(t, second.Retryable)
	assert.Equal(t, 30*time.Second, second.Delay)

	// The third attempt is the last one a 3-attempt job gets, so it stops; a job
	// configured with a higher cap still uses the longest delay.
	assert.False(t, ClassifyDownloadError(transport, 2, DefaultDownloadMaxAttempts).Retryable)
	third := ClassifyDownloadError(transport, 2, 5)
	assert.True(t, third.Retryable)
	assert.Equal(t, 5*time.Minute, third.Delay)
	// Past the table the longest delay is held, not wrapped or dropped.
	assert.Equal(t, 5*time.Minute, ClassifyDownloadError(transport, 4, 10).Delay)
}

func TestClassifyDownloadError_ExhaustedAttemptsStop(t *testing.T) {
	// attempts counts failures BEFORE this one: the third attempt (attempts=2
	// with max 3) is the last, so it must not schedule a fourth.
	assert.True(t, ClassifyDownloadError(errors.New("boom"), 1, DefaultDownloadMaxAttempts).Retryable)
	assert.False(t, ClassifyDownloadError(errors.New("boom"), 2, DefaultDownloadMaxAttempts).Retryable)
	assert.False(t, ClassifyDownloadError(errors.New("boom"), 5, DefaultDownloadMaxAttempts).Retryable)
}

func TestClassifyDownloadError_NilIsNoRetry(t *testing.T) {
	assert.False(t, ClassifyDownloadError(nil, 0, DefaultDownloadMaxAttempts).Retryable)
}

func TestClassifyDownloadError_WrappedSentinelsAreSeen(t *testing.T) {
	wrapped := fmt.Errorf("download: http 404: %w", ErrDownloadPermanent)
	assert.False(t, ClassifyDownloadError(wrapped, 0, DefaultDownloadMaxAttempts).Retryable)
}

func TestResolveDownloadQuality(t *testing.T) {
	assert.Equal(t, QualityFlac, ResolveDownloadQuality(QualityFlac))
	assert.Equal(t, Quality128k, ResolveDownloadQuality(Quality128k))
	// Empty and garbage fall back rather than reaching the resolver, which
	// would silently pick its own default and hide the bad value.
	assert.Equal(t, DefaultDownloadQuality, ResolveDownloadQuality(""))
	assert.Equal(t, DefaultDownloadQuality, ResolveDownloadQuality("lossless"))
}

func TestResolveNamingOrder(t *testing.T) {
	assert.Equal(t, NamingOrderArtistTitle, ResolveNamingOrder(NamingOrderArtistTitle))
	assert.Equal(t, NamingOrderTitleArtist, ResolveNamingOrder(NamingOrderTitleArtist))
	// Empty and unknown values fall back instead of producing a nameless file.
	assert.Equal(t, DefaultNamingOrder, ResolveNamingOrder(""))
	assert.Equal(t, DefaultNamingOrder, ResolveNamingOrder("album-artist-title"))
}

func TestNormalizeNamingOrder(t *testing.T) {
	for _, in := range []string{"", NamingOrderTitleArtist, NamingOrderArtistTitle} {
		got, err := NormalizeNamingOrder(in)
		assert.NoError(t, err)
		assert.Equal(t, ResolveNamingOrder(in), got)
	}
	_, err := NormalizeNamingOrder("album-artist-title")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), NamingOrderArtistTitle, "the error names the allowed values")
}

// TestResolveDownloadContent pins the rule the settings page and the pipeline
// share: a translation of lyrics that were never fetched cannot exist, so asking
// for it implies nothing.
func TestResolveDownloadContent(t *testing.T) {
	cases := []struct {
		name     string
		settings *AppSettings
		want     DownloadContent
	}{
		{
			name:     "nil settings is the zero value",
			settings: nil,
			want:     DownloadContent{},
		},
		{
			name:     "everything off",
			settings: &AppSettings{},
			want:     DownloadContent{},
		},
		{
			name:     "cover only",
			settings: &AppSettings{DownloadWithCover: true},
			want:     DownloadContent{Cover: true},
		},
		{
			name:     "lyrics without translation",
			settings: &AppSettings{DownloadWithLyrics: true},
			want:     DownloadContent{Lyrics: true},
		},
		{
			name:     "translation implies lyrics",
			settings: &AppSettings{DownloadWithLyrics: true, DownloadWithLyricTranslation: true},
			want:     DownloadContent{Lyrics: true, LyricTranslation: true},
		},
		{
			name: "translation alone collapses to nothing",
			// The contradiction: the user asked to keep a translation but not
			// the lyrics. There is nothing to translate, so both are false.
			settings: &AppSettings{DownloadWithLyricTranslation: true},
			want:     DownloadContent{},
		},
		{
			name:     "all three",
			settings: &AppSettings{DownloadWithLyrics: true, DownloadWithLyricTranslation: true, DownloadWithCover: true},
			want:     DownloadContent{Lyrics: true, LyricTranslation: true, Cover: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveDownloadContent(tc.settings)
			assert.Equal(t, tc.want, got)
		})
	}
}
