package wails

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

type countingResponseWriter struct {
	http.ResponseWriter
	writes int
}

func (w *countingResponseWriter) Write(data []byte) (int, error) {
	w.writes++
	return w.ResponseWriter.Write(data)
}

func TestCoverCacheAssetIsServedFromItsDirectory(t *testing.T) {
	dir := t.TempDir()
	name := "abc123.jpg"
	requireWrite(t, filepath.Join(dir, name), []byte("image-bytes"))

	handler := NewAssetHandler(fstest.MapFS{
		"frontend/dist/index.html": &fstest.MapFile{Data: []byte("index")},
	}, nil, dir)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/cover-cache/"+name, nil))

	if response.Code != http.StatusOK || response.Body.String() != "image-bytes" {
		t.Fatalf("cover asset: status %d, body %q", response.Code, response.Body.String())
	}
}

func TestCoverCacheRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	requireWrite(t, filepath.Join(filepath.Dir(dir), "secret.txt"), []byte("secret"))

	handler := NewAssetHandler(fstest.MapFS{
		"frontend/dist/index.html": &fstest.MapFile{Data: []byte("index")},
	}, nil, dir)

	for _, target := range []string{"/cover-cache/../secret.txt", "/cover-cache/..%2Fsecret.txt"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", target, response.Code)
		}
	}
}

func requireWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestServeArtworkVariantSingleWrite(t *testing.T) {
	data := bytes.Repeat([]byte("jpeg"), 20_000)
	response := httptest.NewRecorder()
	w := &countingResponseWriter{ResponseWriter: response}
	serveArtworkVariant(w, httptest.NewRequest(http.MethodGet, "/artwork/test.jpg?size=md", nil), data)

	if w.writes != 1 || !bytes.Equal(response.Body.Bytes(), data) {
		t.Fatalf("variant response: %d writes, %d of %d bytes", w.writes, response.Body.Len(), len(data))
	}
	if response.Header().Get("Content-Type") != "image/jpeg" || response.Header().Get("Content-Length") != "80000" {
		t.Fatalf("variant headers: %v", response.Header())
	}
}
