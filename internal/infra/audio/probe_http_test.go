//go:build ffmpeg_integration

// Verifies the vendored ffmpeg build serves the stream-playback path: a track
// URI served over local http must both probe and decode. This is the M1 check
// from docs/specs/stream-playback/spec.md (ffmpeg only ever talks http to the
// StreamCache proxy; TLS stays in Go).
//
// Run with: go test -tags ffmpeg_integration ./internal/infra/audio/...
package audio

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// serveWAV exposes a generated sine WAV over local http. http.ServeContent
// honours Range requests, which ffmpeg uses to seek inside the stream.
func serveWAV(t *testing.T, seconds float64) *httptest.Server {
	t.Helper()
	data, err := os.ReadFile(writeSineWAV(t, 440, seconds, 44100))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "tone.wav", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDurationProbe_ReadsAnHTTPURL(t *testing.T) {
	srv := serveWAV(t, 3.0)

	got, err := NewDurationProbe().Probe(context.Background(), srv.URL+"/tone.wav")
	if err != nil {
		t.Fatalf("Probe over http: %v", err)
	}
	if math.Abs(got.Seconds()-3.0) > 0.1 {
		t.Fatalf("duration = %.3fs, want about 3s", got.Seconds())
	}
}

func TestAnalyzer_DecodesAnHTTPURL(t *testing.T) {
	srv := serveWAV(t, 1.5)

	feat, err := NewLoudnessAnalyzer().Analyze(context.Background(), srv.URL+"/tone.wav")
	if err != nil {
		t.Fatalf("Analyze over http: %v", err)
	}
	if feat == nil {
		t.Fatal("Analyze returned no features")
	}
	// A 440 Hz sine at full scale must produce a sane loudness figure; the exact
	// value matters less than proving the bytes decoded through the network.
	if feat.LoudnessLUFS >= 0 {
		t.Fatalf("loudness = %.1f LUFS, want a negative integrated loudness", feat.LoudnessLUFS)
	}
}
