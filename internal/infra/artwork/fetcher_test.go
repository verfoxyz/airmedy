package artwork

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	return buf.Bytes()
}

func TestCoverFetcher_ReturnsImageBytesAndType(t *testing.T) {
	body := tinyPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	data, mimeType, err := NewCoverFetcher().Fetch(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "image/png", mimeType)
	assert.Equal(t, body, data)
}

func TestCoverFetcher_EmptyURLIsNotAnError(t *testing.T) {
	data, mimeType, err := NewCoverFetcher().Fetch(context.Background(), "")
	require.NoError(t, err)
	assert.Nil(t, data)
	assert.Empty(t, mimeType)
}

func TestCoverFetcher_RejectsNonImageBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An error page served with 200 must never be embedded as a cover.
		_, _ = w.Write([]byte("<html>nope</html>"))
	}))
	t.Cleanup(srv.Close)

	_, _, err := NewCoverFetcher().Fetch(context.Background(), srv.URL)
	assert.Error(t, err)
}

func TestCoverFetcher_RejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte{0x89}, maxCoverBytes+10))
	}))
	t.Cleanup(srv.Close)

	_, _, err := NewCoverFetcher().Fetch(context.Background(), srv.URL)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "exceeds"), "got %v", err)
}

func TestCoverFetcher_BadStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	_, _, err := NewCoverFetcher().Fetch(context.Background(), srv.URL)
	assert.Error(t, err)
}
