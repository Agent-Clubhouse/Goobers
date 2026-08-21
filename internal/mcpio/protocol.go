package mcpio

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// supportedProtocolVersions lists every MCP revision this server implements,
// newest first. Order is load-bearing: index 0 is both what negotiation
// offers a client whose requested revision we don't implement, and what the
// spec means by "the latest version supported by the server".
//
// 2025-11-25 is listed here only after auditing that revision's published
// delta against 2025-06-18 rather than assuming it (#3457). Every change in
// it is one of: additive-optional on shapes we already emit (tool `title`,
// `icons`, `outputSchema`, `annotations`, `execution`, and
// `Implementation.description` are all optional); opt-in behind a server
// capability this server does not declare (`tasks`); or scoped to features
// and transports this server does not implement at all (authorization-server
// / OAuth discovery, elicitation, sampling, Streamable HTTP and its SSE
// polling rules). Two items were checked specifically because they could
// have bitten a tools-only server: `inputSchema` now defaults to JSON Schema
// 2020-12 when no `$schema` is present, and every schema in toolDefs is
// plain type/properties/required/additionalProperties, which is valid
// 2020-12; and SEP-1303 asks that tool *input validation* failures be
// reported as tool execution errors rather than protocol errors, which is
// already what callTool does via the isError result. What this server
// actually puts on the wire — a stdio session declaring only
// `{"tools":{}}`, tool defs of name/description/inputSchema, and
// `{"content":[{"type":"text",...}],"isError":...}` results — is a
// conformant subset of 2025-11-25 as written.
var supportedProtocolVersions = []string{
	"2025-11-25",
	"2025-06-18",
	"2024-11-05",
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

// Server speaks the MCP stdio JSON-RPC transport: newline-delimited JSON-RPC
// requests on stdin, newline-delimited responses on stdout. It never buffers
// more than one message and never talks to anything but Toolset.
type Server struct {
	tools *Toolset
	// loggedNegotiation makes the negotiated-version log line once-per-session
	// rather than once-per-initialize, since a client is free to retry.
	loggedNegotiation bool
}

// NewServer builds a Server over an already-constructed Toolset.
func NewServer(tools *Toolset) *Server {
	return &Server{tools: tools}
}

// Serve runs the read-dispatch-write loop until stdin closes or an
// unrecoverable I/O error occurs. Malformed individual messages are logged
// to stderr and skipped — the same JSON-RPC-per-line framing this protocol
// requires means one bad line must not take down the whole session.
func (s *Server) Serve(stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	enc := json.NewEncoder(stdout)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytesTrimSpace(line)) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_, _ = fmt.Fprintf(stderr, "mcpio: invalid JSON-RPC line: %v\n", err)
			continue
		}
		resp, ok := s.handle(req, stderr)
		if !ok {
			// Notification (no id) — MCP forbids a response.
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("mcpio: write response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("mcpio: read stdin: %w", err)
	}
	return nil
}

func bytesTrimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// handle dispatches one request. ok is false for notifications, which must
// never get a response on the wire.
func (s *Server) handle(req rpcRequest, stderr io.Writer) (rpcResponse, bool) {
	isNotification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		negotiated, rpcErr := negotiateProtocolVersion(req.Params)
		if rpcErr != nil {
			return s.reply(req, nil, rpcErr), !isNotification
		}
		s.logNegotiation(stderr, negotiated)
		return s.reply(req, map[string]interface{}{
			"protocolVersion": negotiated.agreed,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "goobers-io", "version": "1"},
		}, nil), !isNotification
	case "notifications/initialized", "notifications/cancelled":
		return rpcResponse{}, false
	case "tools/list":
		return s.reply(req, map[string]interface{}{"tools": toolDefs()}, nil), !isNotification
	case "tools/call":
		result, err := s.callTool(req.Params)
		if err != nil {
			return s.reply(req, map[string]interface{}{
				"content": []map[string]interface{}{{"type": "text", "text": err.Error()}},
				"isError": true,
			}, nil), !isNotification
		}
		return s.reply(req, result, nil), !isNotification
	default:
		if isNotification {
			return rpcResponse{}, false
		}
		return s.reply(req, nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}), true
	}
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// negotiationResult is what one initialize handshake settled on. requested
// and agreed differ exactly when the server had to negotiate down to a
// revision of its own.
type negotiationResult struct {
	requested string
	agreed    string
}

// negotiateProtocolVersion implements the MCP lifecycle's version
// negotiation: if the server supports the requested protocol version it MUST
// respond with the same version, and otherwise it MUST respond with another
// protocol version it supports — which SHOULD be the latest one it has. The
// client, not the server, then decides whether that answer is workable and
// disconnects if it isn't.
//
// Answering an unrecognized-but-well-formed version with a JSON-RPC error
// breaks that MUST, and the practical consequence is total rather than
// pedantic: the client is handed no version to fall back to, so it drops the
// server outright. That was #3457. Copilot CLI 1.0.80 asks for a revision
// newer than anything this server listed, the -32602 failed the handshake,
// and every agentic stage on the affected build silently lost the entire
// goobers-io toolset (list_inputs, read_input, grep_input, publish_output,
// get_run_info) — 50 of 50 invocations on the live pod. Adding the new
// revision to supportedProtocolVersions alone would have fixed that day and
// re-broken on the next CLI bump; negotiating is what makes it durable.
//
// So an unfamiliar version string is a negotiation input, not an error. The
// -32602 is reserved for an initialize that carries no usable
// protocolVersion at all — absent, empty, or not a JSON string — where there
// is genuinely nothing to negotiate from and no answer would be honest.
func negotiateProtocolVersion(raw json.RawMessage) (negotiationResult, *rpcError) {
	var params initializeParams
	if len(raw) == 0 {
		return negotiationResult{}, &rpcError{Code: -32602, Message: "initialize requires protocolVersion"}
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return negotiationResult{}, &rpcError{Code: -32602, Message: "invalid initialize params: " + err.Error()}
	}
	if params.ProtocolVersion == "" {
		return negotiationResult{}, &rpcError{Code: -32602, Message: "initialize requires protocolVersion"}
	}
	for _, supported := range supportedProtocolVersions {
		if params.ProtocolVersion == supported {
			return negotiationResult{requested: params.ProtocolVersion, agreed: supported}, nil
		}
	}
	return negotiationResult{
		requested: params.ProtocolVersion,
		agreed:    supportedProtocolVersions[0],
	}, nil
}

// logNegotiation reports, once per session, what the client asked for and
// what the server answered with.
//
// This exists because of how #3457 was actually found. The handshake had
// been failing for seven observed occurrences before anyone traced it, and
// the reason it took that long is that the Goobers side said nothing at all:
// the rejection was written only to the CLI's own private log under
// ~/.copilot/logs, outside the run's artifact tree, so every search of
// events.jsonl and artifacts/ came back empty. stderr is the right sink —
// it's captured with the rest of the stage's harness output, it needs no new
// journal event type, and the MCP stdio transport explicitly permits a
// server to use stderr for logging of any kind, not just errors. A future
// client asking for something this server has never heard of now leaves a
// visible trace on our side instead of only on theirs.
func (s *Server) logNegotiation(stderr io.Writer, result negotiationResult) {
	if s.loggedNegotiation {
		return
	}
	s.loggedNegotiation = true
	if result.agreed == result.requested {
		_, _ = fmt.Fprintf(stderr,
			"mcpio: MCP protocol version negotiated: requested=%s agreed=%s\n",
			result.requested, result.agreed)
		return
	}
	_, _ = fmt.Fprintf(stderr,
		"mcpio: MCP protocol version negotiated: requested=%s agreed=%s"+
			" (requested version not implemented by this server; offered newest of: %s)\n",
		result.requested, result.agreed, strings.Join(supportedProtocolVersions, ", "))
}

func (s *Server) reply(req rpcRequest, result interface{}, rpcErr *rpcError) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr}
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) callTool(raw json.RawMessage) (map[string]interface{}, error) {
	var params toolCallParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("invalid tools/call params: %w", err)
	}
	switch params.Name {
	case "get_run_info":
		data, err := json.Marshal(s.tools.GetRunInfo())
		if err != nil {
			return nil, err
		}
		return textResult(string(data)), nil

	case "publish_output":
		var args struct {
			Content string `json:"content"`
		}
		if err := unmarshalArgs(params.Arguments, &args); err != nil {
			return nil, err
		}
		n, err := s.tools.PublishOutput(args.Content)
		if err != nil {
			return nil, err
		}
		return textResult(fmt.Sprintf(`{"status":"ok","bytesWritten":%d}`, n)), nil

	case "list_inputs":
		items, err := s.tools.ListInputs()
		receipt := InputInspectionReceipt{Tool: "list_inputs", Success: err == nil}
		if err != nil {
			receipt.Error = err.Error()
		}
		if receiptErr := s.tools.recordInputInspection(receipt); receiptErr != nil {
			err = errors.Join(err, receiptErr)
		}
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		return textResult(string(data)), nil

	case "read_input":
		var args struct {
			Name      string `json:"name"`
			StartLine int    `json:"startLine"`
			EndLine   int    `json:"endLine"`
		}
		if err := unmarshalArgs(params.Arguments, &args); err != nil {
			receiptErr := s.tools.recordInputInspection(InputInspectionReceipt{
				Tool: "read_input", Success: false, Error: err.Error(),
			})
			return nil, errors.Join(err, receiptErr)
		}
		result, err := s.tools.ReadInput(args.Name, args.StartLine, args.EndLine)
		receipt := InputInspectionReceipt{
			Tool: "read_input", Input: args.Name, Success: err == nil,
		}
		if err == nil {
			receipt.InputDigest, err = s.tools.inputDigest(args.Name)
			receipt.Success = err == nil
			receipt.StartLine = result.StartLine
			receipt.EndLine = result.EndLine
			receipt.TotalLines = result.TotalLines
			receipt.Truncated = result.Truncated
		}
		if err != nil {
			receipt.Error = err.Error()
		}
		if receiptErr := s.tools.recordInputInspection(receipt); receiptErr != nil {
			err = errors.Join(err, receiptErr)
		}
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return textResult(string(data)), nil

	case "grep_input":
		var args struct {
			Name         string `json:"name"`
			Pattern      string `json:"pattern"`
			ContextLines int    `json:"contextLines"`
		}
		if err := unmarshalArgs(params.Arguments, &args); err != nil {
			receiptErr := s.tools.recordInputInspection(InputInspectionReceipt{
				Tool: "grep_input", Success: false, Error: err.Error(),
			})
			return nil, errors.Join(err, receiptErr)
		}
		result, err := s.tools.GrepInput(args.Name, args.Pattern, args.ContextLines)
		receipt := InputInspectionReceipt{
			Tool: "grep_input", Input: args.Name, Pattern: args.Pattern, Success: err == nil,
		}
		if err == nil {
			receipt.InputDigest, err = s.tools.inputDigest(args.Name)
			receipt.Success = err == nil
			receipt.Truncated = result.Truncated
			receipt.MatchLines = make([]int, 0, len(result.Matches))
			for _, match := range result.Matches {
				receipt.MatchLines = append(receipt.MatchLines, match.LineNumber)
			}
		}
		if err != nil {
			receipt.Error = err.Error()
		}
		if receiptErr := s.tools.recordInputInspection(receipt); receiptErr != nil {
			err = errors.Join(err, receiptErr)
		}
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return textResult(string(data)), nil

	default:
		return nil, fmt.Errorf("unknown tool: %s", params.Name)
	}
}

func unmarshalArgs(raw json.RawMessage, v interface{}) error {
	if len(raw) == 0 {
		return fmt.Errorf("missing arguments")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func textResult(text string) map[string]interface{} {
	return map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": text}},
	}
}

func toolDefs() []toolDef {
	return []toolDef{
		{
			Name:        "get_run_info",
			Description: "Return this agentic stage's run identity: runId, workflowId, taskId, and gaggle.",
			InputSchema: map[string]interface{}{
				"type":                 "object",
				"properties":           map[string]interface{}{},
				"additionalProperties": false,
			},
		},
		{
			Name:        "publish_output",
			Description: "Write this stage's declared output content. Replaces writing a file yourself — call this with your finished output instead of using a file-editing tool, so it's reliably picked up by the runner.",
			InputSchema: map[string]interface{}{
				"type":                 "object",
				"properties":           map[string]interface{}{"content": map[string]interface{}{"type": "string", "description": "The complete output content."}},
				"required":             []string{"content"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "list_inputs",
			Description: "List every upstream input available to this stage, with each one's size in bytes and lines. For a large input, prefer grep_input or a line-ranged read_input over reading the whole thing in one call.",
			InputSchema: map[string]interface{}{
				"type":                 "object",
				"properties":           map[string]interface{}{},
				"additionalProperties": false,
			},
		},
		{
			Name:        "read_input",
			Description: "Read a named input from list_inputs. Omit startLine/endLine for the whole input if it's small; a large input is truncated with totalLines reported so you know to request more. Pass a line range — e.g. from grep_input's contextStart/contextEnd — to read a specific section.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":      map[string]interface{}{"type": "string"},
					"startLine": map[string]interface{}{"type": "integer", "description": "1-based, inclusive. Omit for the start of the input."},
					"endLine":   map[string]interface{}{"type": "integer", "description": "1-based, inclusive. Omit for the end of the input, capped by a default line limit if that's very large."},
				},
				"required":             []string{"name"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "grep_input",
			Description: "Search a named input for a regular expression. Returns each matching line's number and a contextStart/contextEnd window you can pass straight to read_input to see full context around the match.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":         map[string]interface{}{"type": "string"},
					"pattern":      map[string]interface{}{"type": "string", "description": "A regular expression (RE2 syntax)."},
					"contextLines": map[string]interface{}{"type": "integer", "description": "Lines of context to report around each match via contextStart/contextEnd; does not affect what matches. Default 0."},
				},
				"required":             []string{"name", "pattern"},
				"additionalProperties": false,
			},
		},
	}
}
