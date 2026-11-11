package lastfm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free port: %v", err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

func testAuthService(port int, timeout time.Duration) *LastFmService {
	return &LastFmService{
		logger:       slog.Default(),
		apiKey:       "test-key",
		apiSecret:    "test-secret",
		openURL:      func(string) error { return nil },
		callbackPort: port,
		callbackPath: lastFmCallbackPath,
		authTimeout:  timeout,
		limiter:      newRateLimiter(3, 0),
	}
}

func assertPortFree(t *testing.T, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d was not released: %v", port, err)
	}
	_ = listener.Close()
}

func TestLoopbackCallbackDeliversToken(t *testing.T) {
	port := freeLoopbackPort(t)
	service := testAuthService(port, 3*time.Second)

	var status int
	open := func() error {
		response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/auth?token=abc", port))
		if err != nil {
			return fmt.Errorf("callback request: %w", err)
		}
		defer func() { _ = response.Body.Close() }()
		status = response.StatusCode
		return nil
	}

	token, err := service.awaitCallback(context.Background(), open)
	if err != nil {
		t.Fatalf("awaitCallback: %v", err)
	}
	if token != "abc" {
		t.Fatalf("token = %q, want abc", token)
	}
	if status != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", status)
	}
	assertPortFree(t, port)
}

func TestLoopbackCallbackRejectsWrongPathAndMethod(t *testing.T) {
	port := freeLoopbackPort(t)
	service := testAuthService(port, 3*time.Second)
	ctx, cancel := context.WithCancel(context.Background())

	var statuses []int
	open := func() error {
		requests := []struct {
			method string
			path   string
			want   int
		}{
			{http.MethodPost, "/auth?token=abc", http.StatusNotFound},
			{http.MethodGet, "/not-auth?token=abc", http.StatusNotFound},
			{http.MethodGet, "/auth", http.StatusBadRequest},
		}
		for _, spec := range requests {
			request, err := http.NewRequest(spec.method, fmt.Sprintf("http://127.0.0.1:%d%s", port, spec.path), nil)
			if err != nil {
				return err
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				return fmt.Errorf("callback request: %w", err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			statuses = append(statuses, response.StatusCode)
		}
		cancel()
		return nil
	}

	token, err := service.awaitCallback(ctx, open)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("awaitCallback error = %v, want context canceled", err)
	}
	if token != "" {
		t.Fatalf("token = %q, want empty", token)
	}
	if len(statuses) != 3 {
		t.Fatalf("statuses = %v, want three responses", statuses)
	}
	for index, spec := range []int{http.StatusNotFound, http.StatusNotFound, http.StatusBadRequest} {
		if statuses[index] != spec {
			t.Fatalf("status %d = %d, want %d", index, statuses[index], spec)
		}
	}
	assertPortFree(t, port)
}

func TestLoopbackCallbackReportsAuthorizationError(t *testing.T) {
	port := freeLoopbackPort(t)
	service := testAuthService(port, 3*time.Second)

	open := func() error {
		response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/auth?error=access_denied", port))
		if err != nil {
			return fmt.Errorf("callback request: %w", err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return nil
	}

	token, err := service.awaitCallback(context.Background(), open)
	if token != "" {
		t.Fatalf("token = %q, want empty", token)
	}
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("error = %v, want a readable access_denied message", err)
	}
	assertPortFree(t, port)
}

func TestLoopbackCallbackWaitsForATokenAfterAnEmptyOne(t *testing.T) {
	port := freeLoopbackPort(t)
	service := testAuthService(port, 3*time.Second)

	open := func() error {
		first, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/auth", port))
		if err != nil {
			return fmt.Errorf("first callback request: %w", err)
		}
		_ = first.Body.Close()

		second, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/auth?token=later", port))
		if err != nil {
			return fmt.Errorf("second callback request: %w", err)
		}
		_ = second.Body.Close()
		return nil
	}

	token, err := service.awaitCallback(context.Background(), open)
	if err != nil {
		t.Fatalf("awaitCallback: %v", err)
	}
	if token != "later" {
		t.Fatalf("token = %q, want the tokenless redirect to be ignored", token)
	}
	assertPortFree(t, port)
}

func TestLoopbackCallbackTimesOutAndReleasesThePort(t *testing.T) {
	port := freeLoopbackPort(t)
	service := testAuthService(port, 150*time.Millisecond)

	_, err := service.awaitCallback(context.Background(), func() error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	assertPortFree(t, port)
}

func TestLoopbackCallbackCancellationClosesTheListener(t *testing.T) {
	port := freeLoopbackPort(t)
	service := testAuthService(port, 3*time.Second)
	ctx, cancel := context.WithCancel(context.Background())

	_, err := service.awaitCallback(ctx, func() error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	assertPortFree(t, port)
}

func TestConnectReportsAPortAlreadyInUse(t *testing.T) {
	port := freeLoopbackPort(t)
	occupied, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	service := testAuthService(port, time.Second)
	opened := false
	service.openURL = func(string) error {
		opened = true
		return nil
	}

	err = service.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("error = %v, want a readable port-in-use message", err)
	}
	if opened {
		t.Fatal("the browser must not open when the callback port is unavailable")
	}
}

func TestConnectClosesTheListenerWhenTheBrowserFails(t *testing.T) {
	port := freeLoopbackPort(t)
	service := testAuthService(port, 3*time.Second)
	service.openURL = func(string) error { return errors.New("no browser") }

	err := service.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "failed to open browser") {
		t.Fatalf("error = %v, want a browser failure", err)
	}
	assertPortFree(t, port)
}

func TestLoopbackRejectsAConcurrentLogin(t *testing.T) {
	port := freeLoopbackPort(t)
	service := testAuthService(port, 3*time.Second)
	ctx, cancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := service.awaitCallback(ctx, func() error {
			close(started)
			return nil
		})
		done <- err
	}()
	<-started

	_, err := service.awaitCallback(context.Background(), func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("error = %v, want a repeated login to be refused", err)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("first login error = %v, want context canceled", err)
	}
	assertPortFree(t, port)
}
