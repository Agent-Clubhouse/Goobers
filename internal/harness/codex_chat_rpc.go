package harness

// JSONL request correlation and bidirectional dispatch follow Clubhouse's
// codex-app-server-client.ts and ndjson-rpc-base.ts, commit 3309665c (MIT).
// See internal/codexprotocol/CLUBHOUSE-LICENSE.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const codexChatFrameLimit = 16 << 20

type codexRPCMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *codexRPCError  `json:"error,omitempty"`
}

type codexRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *codexRPCError) Error() string {
	return fmt.Sprintf("codex app-server RPC error %d: %s", e.Code, boundedCodexText(e.Message))
}

type codexRPCWrite struct {
	data []byte
	done chan error
}

type codexRPC struct {
	input    io.WriteCloser
	mu       sync.Mutex
	pending  map[string]chan codexRPCMessage
	next     atomic.Uint64
	writes   chan codexRPCWrite
	done     chan struct{}
	readDone chan struct{}
	once     sync.Once
	err      error
	receive  func(codexRPCMessage)
	failed   func(error)
}

func newCodexRPC(input io.WriteCloser, receive func(codexRPCMessage), failed func(error)) *codexRPC {
	r := &codexRPC{
		input: input, pending: map[string]chan codexRPCMessage{},
		writes: make(chan codexRPCWrite, 64), done: make(chan struct{}),
		readDone: make(chan struct{}), receive: receive, failed: failed,
	}
	return r
}

func (r *codexRPC) start(output io.Reader) {
	go r.writeLoop()
	go r.readLoop(output)
}

func (r *codexRPC) fail(err error) {
	r.once.Do(func() {
		r.mu.Lock()
		r.err = err
		r.pending = map[string]chan codexRPCMessage{}
		r.mu.Unlock()
		close(r.done)
		if r.failed != nil {
			// The owner captures descendants before closing stdin can make the
			// parent exit and orphan them. Close performs the final pipe close.
			go r.failed(err)
		} else {
			_ = r.input.Close()
		}
	})
}

func (r *codexRPC) terminalError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *codexRPC) request(ctx context.Context, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id := fmt.Sprint(r.next.Add(1))
	ch := make(chan codexRPCMessage, 1)
	r.mu.Lock()
	r.pending[id] = ch
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
	}()
	if err := r.send(ctx, codexRPCMessage{ID: json.RawMessage(id), Method: method, Params: codexRaw(params)}); err != nil {
		return err
	}
	select {
	case response := <-ch:
		if response.Error != nil {
			return response.Error
		}
		if result != nil {
			if err := json.Unmarshal(response.Result, result); err != nil {
				r.fail(errors.New("codex app-server returned an invalid response"))
				return r.terminalError()
			}
		}
		return nil
	case <-ctx.Done():
		// Delivery is uncertain. Never leave an unacknowledged turn running
		// while allowing the host to retry or hand off the workspace.
		r.fail(fmt.Errorf("codex app-server %s interrupted: %w", method, ctx.Err()))
		return r.terminalError()
	case <-r.done:
		return r.terminalError()
	}
}

func (r *codexRPC) send(ctx context.Context, msg codexRPCMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if len(data) > codexChatFrameLimit {
		return errors.New("codex app-server outbound frame exceeds limit")
	}
	write := codexRPCWrite{data: append(data, '\n'), done: make(chan error, 1)}
	select {
	case <-r.done:
		return r.terminalError()
	case <-ctx.Done():
		return ctx.Err()
	case r.writes <- write:
	}
	select {
	case err := <-write.done:
		return err
	case <-r.done:
		return r.terminalError()
	case <-ctx.Done():
		r.fail(ctx.Err())
		return ctx.Err()
	}
}

func (r *codexRPC) writeLoop() {
	for {
		select {
		case <-r.done:
			return
		case w := <-r.writes:
			n, err := r.input.Write(w.data)
			if err == nil && n != len(w.data) {
				err = io.ErrShortWrite
			}
			w.done <- err
			if err != nil {
				r.fail(errors.New("codex app-server input closed"))
				return
			}
		}
	}
}

func (r *codexRPC) readLoop(output io.Reader) {
	defer close(r.readDone)
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), codexChatFrameLimit)
	for scanner.Scan() {
		var msg codexRPCMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			r.receive(codexRPCMessage{Method: "malformed_frame"})
			continue
		}
		if msg.Method != "" {
			r.receive(msg)
			continue
		}
		r.mu.Lock()
		ch := r.pending[string(msg.ID)]
		r.mu.Unlock()
		if ch == nil {
			r.receive(codexRPCMessage{Method: "unknown_response"})
			continue
		}
		select {
		case ch <- msg:
		default:
			r.receive(codexRPCMessage{Method: "duplicate_response"})
		}
	}
	r.fail(errors.New("codex app-server output closed or exceeded frame limit"))
}

func codexRaw(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func boundedCodexText(text string) string {
	if len(text) > 2048 {
		return text[:2048] + "…"
	}
	return text
}
