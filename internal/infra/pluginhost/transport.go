package pluginhost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"airmedy/internal/domain"
)

// maxMessageSize bounds one JSON-RPC line. Payloads can legitimately be large
// (a playlist page, a lyric), so the buffer is far above bufio.Scanner's 64 KiB
// default, but a runaway line is still rejected instead of exhausting memory.
const maxMessageSize = 8 << 20 // 8 MiB

// rpcMessage is one line of the stdio protocol.
//
// ID is kept raw because JSON-RPC lets either side use a number or a string;
// the host always sends numbers, but a plugin's request may use either and must
// not break the reader.
type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *domain.RPCError `json:"error,omitempty"`
}

// rpcOutcome is what a pending call resolves to.
type rpcOutcome struct {
	result json.RawMessage
	err    error
}

// transport is one newline-delimited JSON-RPC 2.0 session over a plugin's
// stdin/stdout.
//
// The reader owns the lifecycle: when the plugin closes stdout or the pipe
// breaks, every pending call fails with the same crash cause and no further
// call can be issued. Writes are serialised so concurrent Call invocations
// never interleave half a line.
type transport struct {
	pluginID string
	logger   *slog.Logger
	// handler answers plugin → host requests. A nil handler keeps the v1
	// behaviour: every inbound method is -32601.
	handler domain.PluginRequestHandler

	writeMu sync.Mutex
	writer  *bufio.Writer

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan rpcOutcome
	closed  bool
	fatal   error

	done       chan struct{}
	readerDone chan struct{}
	once       sync.Once
}

// newTransport starts reading r in the background. The caller owns closing r
// and w (they are the plugin process pipes).
func newTransport(r io.Reader, w io.Writer, pluginID string, logger *slog.Logger, handler domain.PluginRequestHandler) *transport {
	if logger == nil {
		logger = slog.Default()
	}
	t := &transport{
		pluginID:   pluginID,
		logger:     logger,
		handler:    handler,
		writer:     bufio.NewWriter(w),
		pending:    make(map[uint64]chan rpcOutcome),
		done:       make(chan struct{}),
		readerDone: make(chan struct{}),
	}
	go func() {
		defer close(t.readerDone)
		t.readLoop(r)
	}()
	return t
}

// readLoop consumes one JSON object per line until the stream ends.
func (t *transport) readLoop(r io.Reader) {
	reader := bufio.NewReader(r)
	for {
		line, err := readLine(reader)
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			t.handleLine(trimmed)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.close(fmt.Errorf("%w: plugin closed its stdout", domain.ErrPluginCrashed))
			} else {
				t.close(fmt.Errorf("%w: read plugin stdout: %v", domain.ErrPluginCrashed, err))
			}
			return
		}
	}
}

// readLine reads one newline-terminated message, including the final message
// of a stream that has no trailing newline.
func readLine(reader *bufio.Reader) ([]byte, error) {
	var buffer []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(buffer)+len(chunk) > maxMessageSize {
			return nil, fmt.Errorf("line exceeds %d bytes", maxMessageSize)
		}
		buffer = append(buffer, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return buffer, err
	}
}

// handleLine routes one decoded message: a response resolves a pending call, a
// request from the plugin is answered with "method not found" (the host exposes
// no methods in v1), and a notification is logged and otherwise ignored so an
// unknown event from a newer plugin cannot break an older host.
func (t *transport) handleLine(line []byte) {
	var message rpcMessage
	if err := json.Unmarshal(line, &message); err != nil {
		t.logger.Warn("Ignoring malformed line from plugin", "plugin", t.pluginID, "error", err)
		return
	}

	hasID := len(message.ID) > 0 && !bytes.Equal(bytes.TrimSpace(message.ID), []byte("null"))
	switch {
	case message.Method != "" && hasID:
		// A handler is bound only once the app has something to serve (KV
		// today). Without one, every inbound method keeps the v1 answer
		// -32601, so an old host and an old plugin still agree.
		t.answerInbound(message)
	case message.Method != "":
		t.logger.Debug("Plugin notification", "plugin", t.pluginID, "method", message.Method)
	case hasID:
		t.deliver(message)
	default:
		t.logger.Debug("Ignoring plugin message without method or id", "plugin", t.pluginID)
	}
}

// answerInbound serves one plugin → host request through the bound handler and
// writes the reply.
//
// It runs in the reader goroutine, so it must not block on the plugin: the
// handler touches local stores only, and the reply goes through the same serial
// writer every outbound call uses.
//
// The reply is written with the plugin's own id as the request id, because the
// host has no notion of a second id space: an inbound request is answered by the
// line read, and the answer carries whatever id the plugin chose.
func (t *transport) answerInbound(message rpcMessage) {
	if t.handler == nil {
		t.logger.Debug("Answering unsupported plugin request",
			"plugin", t.pluginID, "method", message.Method)
		t.respond(message.ID, &domain.RPCError{
			Code:    domain.RPCErrorMethodNotFound,
			Message: fmt.Sprintf("host method %q not found", message.Method),
		})
		return
	}

	// A handler that panics must not take the whole session down with it: one
	// bad request would otherwise kill a plugin the user is playing music
	// through. The panic is logged and answered like any other host failure.
	var (
		result any
		err    error
	)
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				// The host's own bug: report it as an internal error and keep
				// the session alive.
				err = fmt.Errorf("host request handler panicked: %v", recovered)
				t.logger.Error("Plugin request handler panicked",
					"plugin", t.pluginID, "method", message.Method, "panic", recovered)
			}
		}()
		result, err = t.handler(context.Background(), t.pluginID, message.Method, message.Params)
	}()

	if err != nil {
		code := domain.RPCErrorCode(err)
		if code == 0 {
			code = domain.RPCErrorInternal
		}
		t.logger.Debug("Refused plugin request",
			"plugin", t.pluginID, "method", message.Method, "code", code, "error", err)
		t.respond(message.ID, &domain.RPCError{Code: code, Message: err.Error()})
		return
	}
	if err := t.writeResult(message.ID, result); err != nil {
		t.logger.Debug("Failed to answer plugin request",
			"plugin", t.pluginID, "method", message.Method, "error", err)
	}
}

// writeResult writes a successful response to a plugin-initiated request.
func (t *transport) writeResult(id json.RawMessage, result any) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	return t.write(rpcMessage{JSONRPC: "2.0", ID: id, Result: encoded})
}

// notify writes a notification (a message with no id), which the plugin must
// not answer. It is used for lifecycle.unload: a request would need a reply to
// correlate, and the process is about to exit anyway.
func (t *transport) notify(method string, params any) error {
	paramsRaw, err := encodeParams(params)
	if err != nil {
		return fmt.Errorf("notify %q: encode params: %w", method, err)
	}
	return t.write(rpcMessage{JSONRPC: "2.0", Method: method, Params: paramsRaw})
}

// deliver resolves the pending call that owns this response id.
func (t *transport) deliver(message rpcMessage) {
	id, err := parseRPCID(message.ID)
	if err != nil {
		t.logger.Warn("Ignoring response with an invalid id",
			"plugin", t.pluginID, "id", string(message.ID), "error", err)
		return
	}

	t.mu.Lock()
	pending, ok := t.pending[id]
	if ok {
		delete(t.pending, id)
	}
	t.mu.Unlock()
	if !ok {
		// A response that arrived after its call timed out is expected and
		// harmless; it must not be treated as a protocol failure.
		t.logger.Debug("Ignoring response for an unknown call", "plugin", t.pluginID, "id", id)
		return
	}

	if message.Error != nil {
		pending <- rpcOutcome{err: message.Error}
		return
	}
	pending <- rpcOutcome{result: message.Result}
}

// call sends one request and waits for its response, the context to end, or the
// session to die.
func (t *transport) call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("plugin call %q: %w", method, err)
	}
	paramsRaw, err := encodeParams(params)
	if err != nil {
		return fmt.Errorf("plugin call %q: encode params: %w", method, err)
	}

	id := t.allocateID()
	outcome := make(chan rpcOutcome, 1)

	t.mu.Lock()
	if t.closed {
		cause := t.fatal
		t.mu.Unlock()
		return callFailure(method, cause)
	}
	t.pending[id] = outcome
	t.mu.Unlock()

	message := rpcMessage{
		JSONRPC: "2.0",
		ID:      json.RawMessage(strconv.FormatUint(id, 10)),
		Method:  method,
		Params:  paramsRaw,
	}
	if err := t.write(message); err != nil {
		t.forget(id)
		return callFailure(method, err)
	}

	select {
	case <-ctx.Done():
		t.forget(id)
		return fmt.Errorf("plugin call %q: %w", method, ctx.Err())
	case <-t.done:
		t.forget(id)
		return callFailure(method, t.cause())
	case reply := <-outcome:
		if reply.err != nil {
			return fmt.Errorf("plugin call %q: %w", method, reply.err)
		}
		if result == nil || len(reply.result) == 0 || bytes.Equal(bytes.TrimSpace(reply.result), []byte("null")) {
			return nil
		}
		if err := json.Unmarshal(reply.result, result); err != nil {
			return fmt.Errorf("plugin call %q: decode result: %w", method, err)
		}
		return nil
	}
}

// respond answers a plugin-initiated request with an error object.
func (t *transport) respond(id json.RawMessage, rpcErr *domain.RPCError) {
	if err := t.write(rpcMessage{JSONRPC: "2.0", ID: id, Error: rpcErr}); err != nil {
		t.logger.Debug("Failed to answer plugin request",
			"plugin", t.pluginID, "error", err)
	}
}

// write serialises and flushes one message.
func (t *transport) write(message rpcMessage) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	if len(encoded) > maxMessageSize {
		return fmt.Errorf("message exceeds %d bytes", maxMessageSize)
	}
	encoded = append(encoded, '\n')

	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.writer.Write(encoded); err != nil {
		return fmt.Errorf("%w: write to plugin stdin: %v", domain.ErrPluginCrashed, err)
	}
	if err := t.writer.Flush(); err != nil {
		return fmt.Errorf("%w: flush plugin stdin: %v", domain.ErrPluginCrashed, err)
	}
	return nil
}

func (t *transport) allocateID() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextID++
	return t.nextID
}

func (t *transport) forget(id uint64) {
	t.mu.Lock()
	delete(t.pending, id)
	t.mu.Unlock()
}

// waitReader blocks until the reader goroutine has returned. It only returns
// once the plugin's stdout pipe is closed, which Host guarantees by reaping the
// process first.
func (t *transport) waitReader() {
	<-t.readerDone
}

// isClosed reports whether the session has already failed or shut down.
func (t *transport) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

func (t *transport) cause() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fatal
}

// close ends the session and fails every pending call with cause. It is
// idempotent; the first cause wins so a graceful shutdown is not overwritten by
// the EOF that follows it.
func (t *transport) close(cause error) {
	t.once.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.fatal = cause
		pending := t.pending
		t.pending = make(map[uint64]chan rpcOutcome)
		t.mu.Unlock()

		close(t.done)
		for _, waiter := range pending {
			waiter <- rpcOutcome{err: cause}
		}
	})
}

// callFailure turns the session cause into an error that keeps the "plugin call
// X" context and still satisfies errors.Is for the protocol sentinels.
func callFailure(method string, cause error) error {
	if cause == nil {
		cause = domain.ErrPluginCrashed
	}
	return fmt.Errorf("plugin call %q: %w", method, cause)
}

// encodeParams JSON-encodes call params, treating nil as "no params".
func encodeParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// parseRPCID accepts a numeric or string id. The host only ever sends numbers;
// strings are tolerated so a plugin that echoes an id oddly still resolves.
func parseRPCID(raw json.RawMessage) (uint64, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0, fmt.Errorf("id is empty")
	}
	if trimmed[0] == '"' {
		var asString string
		if err := json.Unmarshal(raw, &asString); err != nil {
			return 0, err
		}
		trimmed = asString
	}
	id, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("id %q is not numeric: %w", trimmed, err)
	}
	return id, nil
}
