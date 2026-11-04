package updater

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"airmedy/internal/app/config"
)

// countingTransport fails the test if anything reaches the network; disabled
// builds must not even attempt the release request.
type countingTransport struct {
	calls atomic.Int32
}

func (t *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("unexpected network request")
}

func newTestService() *Service {
	return NewService(config.Version, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestCheckForUpdate_DisabledBuildMakesNoRequest(t *testing.T) {
	if !config.UpdatesDisabled {
		t.Skip("in-app updates are enabled in this build")
	}

	original := http.DefaultClient
	transport := &countingTransport{}
	http.DefaultClient = &http.Client{Transport: transport}
	t.Cleanup(func() { http.DefaultClient = original })

	info, err := newTestService().CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if info != nil {
		t.Fatalf("CheckForUpdate returned %+v, want nil", info)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("release feed requests = %d, want 0", got)
	}
}

func TestDownloadAndApply_DisabledBuildRefuses(t *testing.T) {
	if !config.UpdatesDisabled {
		t.Skip("in-app updates are enabled in this build")
	}

	err := newTestService().DownloadAndApply(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "in-app updates are disabled") {
		t.Fatalf("DownloadAndApply = %v, want a disabled-build error", err)
	}
}
