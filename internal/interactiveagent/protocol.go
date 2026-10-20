package interactiveagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
)

var protocolVersions = []string{"2025-11-25", "2025-06-18", "2024-11-05"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type connection struct {
	bridge      *Bridge
	conn        net.Conn
	ctx         context.Context
	cancel      context.CancelFunc
	writeMu     sync.Mutex
	pendingMu   sync.Mutex
	pending     map[string]context.CancelFunc
	initialized bool
	requests    sync.WaitGroup
}

func (b *Bridge) serveConnection(conn net.Conn) {
	if !b.authenticate(conn) {
		return
	}
	ctx, cancel := context.WithCancel(b.ctx)
	defer cancel()
	c := &connection{bridge: b, conn: conn, ctx: ctx, cancel: cancel, pending: map[string]context.CancelFunc{}}
	reader := bufio.NewReaderSize(conn, 4096)
	for {
		line, err := readLine(reader)
		if err != nil {
			if errors.Is(err, ErrInvalidMessage) {
				c.respond(nil, nil, &rpcError{-32600, "MCP message exceeds the request limit"})
			}
			if errors.Is(err, io.EOF) {
				c.requests.Wait()
			}
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var request rpcRequest
		if unsafeText(line) || strictDecode(line, &request) != nil || request.JSONRPC != "2.0" || request.Method == "" {
			c.respond(nil, nil, &rpcError{-32600, "Invalid JSON-RPC request"})
			continue
		}
		if len(request.ID) > 0 {
			if _, ok := rpcID(request.ID); !ok {
				c.respond(nil, nil, &rpcError{-32600, "Invalid request ID"})
				continue
			}
		}
		c.handle(request)
	}
}

func rpcID(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || len(raw) > 256 {
		return "", false
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return "s:" + text, true
	}
	var number json.Number
	if json.Unmarshal(raw, &number) != nil {
		return "", false
	}
	value, err := number.Int64()
	if err != nil {
		return "", false
	}
	return "n:" + strconv.FormatInt(value, 10), true
}

func (c *connection) respond(id json.RawMessage, result any, rpcErr *rpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	response := rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr}
	data, err := json.Marshal(response)
	if err != nil || len(data) > MaxResponseBytes || unsafeText(data) {
		data, _ = json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{-32603, "Tool output could not be safely returned"}})
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err = c.conn.Write(append(data, '\n')); err != nil {
		c.cancel()
		_ = c.conn.Close()
	}
}

func (c *connection) handle(req rpcRequest) {
	notification := len(req.ID) == 0
	if req.Method == "notifications/cancelled" {
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
			Reason    string          `json:"reason,omitempty"`
		}
		if notification && strictDecode(req.Params, &params) == nil {
			if key, ok := rpcID(params.RequestID); ok {
				c.pendingMu.Lock()
				cancel := c.pending[key]
				c.pendingMu.Unlock()
				if cancel != nil {
					cancel()
				}
			}
		}
		return
	}
	if notification {
		return
	} // A notification must never launch an untracked tool effect.
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string          `json:"protocolVersion"`
			Capabilities    json.RawMessage `json:"capabilities,omitempty"`
			ClientInfo      json.RawMessage `json:"clientInfo,omitempty"`
		}
		if strictDecode(req.Params, &params) != nil || params.ProtocolVersion == "" {
			c.respond(req.ID, nil, &rpcError{-32602, "initialize requires protocolVersion"})
			return
		}
		version := protocolVersions[0]
		for _, supported := range protocolVersions {
			if supported == params.ProtocolVersion {
				version = supported
			}
		}
		c.initialized = true
		c.respond(req.ID, map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "goobers-interactive-agent", "version": "1"}}, nil)
	case "ping":
		if !emptyParams(req.Params) {
			c.respond(req.ID, nil, &rpcError{-32602, "Invalid ping parameters"})
			return
		}
		c.respond(req.ID, map[string]any{}, nil)
	case "version":
		if !emptyParams(req.Params) {
			c.respond(req.ID, nil, &rpcError{-32602, "Invalid version parameters"})
			return
		}
		c.respond(req.ID, map[string]any{"name": "goobers-interactive-agent", "version": "1", "protocolVersion": protocolVersions[0]}, nil)
	case "tools/list":
		if !c.initialized {
			c.respond(req.ID, nil, &rpcError{-32000, "Initialize the MCP session first"})
			return
		}
		var params struct {
			Cursor string `json:"cursor,omitempty"`
		}
		if len(req.Params) > 0 && (strictDecode(req.Params, &params) != nil || params.Cursor != "") {
			c.respond(req.ID, nil, &rpcError{-32602, "Invalid tools/list parameters"})
			return
		}
		c.respond(req.ID, map[string]any{"tools": c.bridge.definitions}, nil)
	case "tools/call":
		c.call(req)
	default:
		c.respond(req.ID, nil, &rpcError{-32601, "Method not found"})
	}
}

func (c *connection) call(req rpcRequest) {
	if !c.initialized {
		c.respond(req.ID, nil, &rpcError{-32000, "Initialize the MCP session first"})
		return
	}
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      map[string]any  `json:"_meta,omitempty"`
	}
	if strictDecode(req.Params, &params) != nil || !argumentIdentitySafe(params.Meta) {
		c.respond(req.ID, toolFailure("invalid_arguments"), nil)
		return
	}
	tool, ok := c.bridge.tools[params.Name]
	if !ok {
		c.respond(req.ID, toolFailure("unknown_tool"), nil)
		return
	}
	value, err := uniqueJSON(params.Arguments)
	if err != nil || !argumentIdentitySafe(value) || tool.schema.Validate(value) != nil {
		c.respond(req.ID, toolFailure("invalid_arguments"), nil)
		return
	}
	key, _ := rpcID(req.ID)
	c.pendingMu.Lock()
	if _, exists := c.pending[key]; exists || len(c.pending) >= 32 {
		c.pendingMu.Unlock()
		c.respond(req.ID, nil, &rpcError{-32600, "Request ID is already active or queue is full"})
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.pending[key] = cancel
	c.pendingMu.Unlock()
	c.requests.Add(1)
	call := Call{ID: append(json.RawMessage(nil), req.ID...), Name: params.Name, Arguments: append(json.RawMessage(nil), params.Arguments...)}
	go func() {
		defer c.requests.Done()
		defer cancel()
		defer func() { c.pendingMu.Lock(); delete(c.pending, key); c.pendingMu.Unlock() }()
		select {
		case <-ctx.Done():
			c.respond(req.ID, toolFailure("cancelled"), nil)
			return
		case c.bridge.invokeSlot <- struct{}{}:
		}
		defer func() { <-c.bridge.invokeSlot }()
		if !c.bridge.beginCallback() {
			c.respond(req.ID, toolFailure("cancelled"), nil)
			return
		}
		defer c.bridge.callbacks.Done()
		if ctx.Err() != nil {
			c.respond(req.ID, toolFailure("cancelled"), nil)
			return
		}
		if err := safeCheck(ctx, c.bridge.options.Check); err != nil {
			c.bridge.audit(params.Name, "denied")
			c.respond(req.ID, toolFailure("execution_unavailable"), nil)
			return
		}
		if ctx.Err() != nil {
			c.respond(req.ID, toolFailure("cancelled"), nil)
			return
		}
		c.bridge.audit(params.Name, "started")
		result, err := safeInvoke(ctx, c.bridge.options.Invoke, call)
		if err != nil {
			c.bridge.audit(params.Name, "failed")
			if ctx.Err() != nil {
				c.respond(req.ID, toolFailure("cancelled"), nil)
			} else {
				c.respond(req.ID, toolFailure("tool_failed"), nil)
			}
			return
		}
		data, err := json.Marshal(result)
		if err != nil || len(data) > MaxResponseBytes-1024 || unsafeText(data) {
			c.bridge.audit(params.Name, "failed")
			c.respond(req.ID, toolFailure("unsafe_output"), nil)
			return
		}
		c.bridge.audit(params.Name, "completed")
		c.respond(req.ID, map[string]any{"content": []map[string]string{{"type": "text", "text": string(data)}}, "isError": false}, nil)
	}()
}

func emptyParams(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var params struct{}
	return strictDecode(raw, &params) == nil && bytes.Equal(bytes.TrimSpace(raw), []byte("{}"))
}

func toolFailure(code string) any {
	data, _ := json.Marshal(map[string]string{"code": code, "message": "The host tool did not complete; inspect the portal for its current state."})
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(data)}}, "isError": true}
}
func argumentIdentitySafe(value any) bool {
	switch node := value.(type) {
	case map[string]any:
		for key, v := range node {
			if reservedArgument(key) || !argumentIdentitySafe(v) {
				return false
			}
		}
	case []any:
		for _, v := range node {
			if !argumentIdentitySafe(v) {
				return false
			}
		}
	}
	return true
}
func safeCheck(ctx context.Context, check func(context.Context) error) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrUnavailable
		}
	}()
	return check(ctx)
}
func safeInvoke(ctx context.Context, invoke func(context.Context, Call) (any, error), call Call) (result any, err error) {
	defer func() {
		if recover() != nil {
			result = nil
			err = ErrUnavailable
		}
	}()
	return invoke(ctx, call)
}
func (b *Bridge) audit(tool, phase string) {
	if b.options.Emit != nil {
		func() { defer func() { _ = recover() }(); b.options.Emit(AuditEvent{Tool: tool, Phase: phase}) }()
	}
}
