package lastfm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	// lastFmCallbackPort is the fixed port Last.fm has registered for this app.
	// The callback URL must match the registered string exactly, so this cannot
	// be randomised in production.
	lastFmCallbackPort = 53682
	// lastFmCallbackPath is the registered callback path.
	lastFmCallbackPath = "/auth"
	// authCallbackTimeout is how long a login waits for the browser redirect.
	authCallbackTimeout = 5 * time.Minute
	// authShutdownTimeout bounds the graceful close of the callback server.
	authShutdownTimeout = 3 * time.Second
)

// callbackSuccessHTML is the only page the loopback server ever returns.
//
// It is static on purpose: the response must not echo the token or the API
// secret, and it carries no data the browser could leak back to a third party.
const callbackSuccessHTML = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Last.fm</title></head>
<body style="font-family: sans-serif; text-align: center; padding-top: 15vh">
  <h1>Authorization complete</h1>
  <p>You can close this page and return to Airmedy.</p>
</body>
</html>`

// callbackResult is one terminal answer from the browser redirect.
type callbackResult struct {
	token string
	err   error
}

// callbackURL is the redirect Last.fm must hit. It matches the registered
// callback exactly, including the fixed port and path.
func (s *LastFmService) callbackURL() string {
	port := s.callbackPort
	if port == 0 {
		port = lastFmCallbackPort
	}
	path := s.callbackPath
	if path == "" {
		path = lastFmCallbackPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
}

// awaitCallback starts the loopback listener, opens the browser and waits for
// Last.fm to redirect back with a token.
//
// Only 127.0.0.1 is bound: the callback carries a one-time token and must not be
// reachable from the network. Every return path closes the listener, so a
// timeout, cancellation or browser failure leaves the fixed port free for the
// next attempt.
func (s *LastFmService) awaitCallback(ctx context.Context, open func() error) (string, error) {
	s.authMu.Lock()
	if s.authActive {
		s.authMu.Unlock()
		return "", fmt.Errorf("last.fm login is already in progress")
	}
	s.authActive = true
	s.authMu.Unlock()
	defer func() {
		s.authMu.Lock()
		s.authActive = false
		s.authMu.Unlock()
	}()

	port := s.callbackPort
	if port == 0 {
		port = lastFmCallbackPort
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", fmt.Errorf(
			"last.fm login needs local port %d for its callback, but it is already in use: %w",
			port, err)
	}

	result := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(s.callbackPathOrDefault(), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}

		query := r.URL.Query()
		if code := strings.TrimSpace(query.Get("error")); code != "" {
			http.Error(w, "Last.fm authorization failed.", http.StatusBadRequest)
			deliverCallback(result, callbackResult{err: lastFmAuthorizationError(code)})
			return
		}

		token := strings.TrimSpace(query.Get("token"))
		if token == "" {
			// A tokenless redirect is not the end of the flow: Last.fm may retry,
			// so the handler keeps the listener open and waits for a usable one.
			http.Error(w, "Missing token.", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, callbackSuccessHTML)
		deliverCallback(result, callbackResult{token: token})
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	defer func() {
		// Close the listener first: the Serve goroutine may not have registered it
		// with the server yet, and a cancellation must free the fixed port even in
		// that window.
		_ = listener.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), authShutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		_ = server.Close()
	}()

	if err := open(); err != nil {
		return "", err
	}

	timeout := s.authTimeout
	if timeout <= 0 {
		timeout = authCallbackTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case answer := <-result:
		return answer.token, answer.err
	case err := <-serveErr:
		return "", fmt.Errorf("last.fm callback server failed: %w", err)
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf(
			"last.fm login timed out after %s waiting for the browser callback: %w",
			timeout, waitCtx.Err())
	}
}

func (s *LastFmService) callbackPathOrDefault() string {
	path := s.callbackPath
	if path == "" {
		return lastFmCallbackPath
	}
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

// deliverCallback hands the terminal answer to the waiter without blocking the
// HTTP handler. A second redirect after a successful one is ignored.
func deliverCallback(target chan<- callbackResult, answer callbackResult) {
	select {
	case target <- answer:
	default:
	}
}

// lastFmAuthorizationError words the error code Last.fm put on the redirect.
func lastFmAuthorizationError(code string) error {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "access_denied":
		return fmt.Errorf("last.fm authorization was denied")
	case "invalid_token":
		return fmt.Errorf("last.fm rejected the authorization token")
	default:
		return fmt.Errorf("last.fm authorization failed: %s", code)
	}
}
