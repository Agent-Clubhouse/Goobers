package mcpio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract/childworkflowwire"
	"github.com/goobers/goobers/internal/pathutil"
	"github.com/goobers/goobers/internal/safeio"
)

// ChildWorkflowAccess is a launcher-issued grant for the Config's run. Endpoint
// is a daemon HTTP(S) origin, without userinfo, path, query, or fragment. The
// service verifies the signed grant and active occurrence on every request.
// Keep this in the private runtime config; never render it into prompts/argv.
type ChildWorkflowAccess struct {
	Endpoint    string `json:"endpoint"`
	BearerToken string `json:"bearerToken"`
}

// String redacts access from incidental formatted diagnostics.
func (a ChildWorkflowAccess) String() string { return "<child-workflow access>" }

// GoString redacts access from Go-syntax formatted diagnostics.
func (a ChildWorkflowAccess) GoString() string { return a.String() }

// Validate rejects malformed trusted configuration without echoing its secrets.
func (a ChildWorkflowAccess) Validate(run string) error {
	endpoint, err := url.Parse(a.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.RawPath != "" ||
		(endpoint.Path != "" && endpoint.Path != "/") || !apiv1.ValidRunID(run) {
		return errors.New("mcpio: invalid child workflow endpoint or run binding")
	}
	if !strings.HasPrefix(a.BearerToken, "goobers-child.") || len(a.BearerToken) <= len("goobers-child.") || len(a.BearerToken) > 4096 || strings.IndexFunc(a.BearerToken, unicode.IsSpace) >= 0 || strings.IndexFunc(a.BearerToken, unicode.IsControl) >= 0 {
		return errors.New("mcpio: invalid child workflow grant")
	}
	return nil
}

// ChildWorkflowToolNames returns fresh names only for a complete trusted grant.
func ChildWorkflowToolNames(access *ChildWorkflowAccess, run string) []string {
	if access == nil || access.Validate(run) != nil {
		return nil
	}
	return []string{"validate_child_workflow", "start_child_workflow", "get_child_workflow"}
}

func isChildWorkflowTool(name string) bool {
	return name == "validate_child_workflow" || name == "start_child_workflow" || name == "get_child_workflow"
}

const childWorkflowTimeout = 10 * time.Second // HTTP server budget is eight seconds.
const maxChildWorkflowResponseBytes = 256 << 10

func (s *Server) callChildWorkflowTool(name string, raw json.RawMessage) (map[string]interface{}, error) {
	if len(ChildWorkflowToolNames(s.tools.cfg.ChildWorkflows, s.tools.cfg.RunID)) == 0 {
		return nil, errors.New("child workflow tools are unavailable for this stage")
	}
	args, err := childWorkflowArgs(name, raw)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), childWorkflowTimeout)
	defer cancel()
	var result any
	switch name {
	case "validate_child_workflow":
		source, err := s.tools.childWorkflowSource(args["sourceFile"])
		if err != nil {
			return nil, err
		}
		var response childworkflowwire.ChildWorkflowValidationResponse
		err = s.tools.callChildWorkflow(ctx, childworkflowwire.ValidatePath, "", childworkflowwire.ChildWorkflowSourceRequest{Source: string(source)}, &response)
		if err != nil {
			return nil, err
		}
		result = response
	case "start_child_workflow":
		source, err := s.tools.childWorkflowSource(args["sourceFile"])
		if err != nil {
			return nil, err
		}
		var response childworkflowwire.ChildWorkflowResponse
		err = s.tools.callChildWorkflow(ctx, childworkflowwire.StartPath, args["invocationKey"], childworkflowwire.ChildWorkflowSourceRequest{Source: string(source)}, &response)
		if err != nil {
			return nil, err
		}
		result = response
	case "get_child_workflow":
		var response childworkflowwire.ChildWorkflowResponse
		err = s.tools.callChildWorkflow(ctx, childworkflowwire.StatusPath, "", childworkflowwire.ChildWorkflowStatusRequest{InvocationKey: args["invocationKey"]}, &response)
		if err != nil {
			return nil, err
		}
		result = response
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, errors.New("cannot encode child workflow result")
	}
	return textResult(strings.ReplaceAll(string(data), s.tools.cfg.ChildWorkflows.BearerToken, "[REDACTED]")), nil
}

func (t *Toolset) childWorkflowSource(rel string) ([]byte, error) {
	clean, err := pathutil.IsLexicallyContained("", rel)
	if err != nil || strings.Contains(rel, `\`) {
		return nil, errors.New("sourceFile must be a workspace-relative file")
	}
	// Never turn runtime configuration, credentials or git control data into DSL.
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".goobers" || part == ".git" {
			return nil, errors.New("sourceFile must be outside runtime and git control directories")
		}
	}
	full, err := t.resolveInWorkspace(clean, false)
	if err != nil {
		return nil, errors.New("sourceFile must be a regular file inside the workspace")
	}
	root, err := filepath.EvalSymlinks(t.cfg.Workspace)
	if err != nil {
		return nil, errors.New("source workspace is unavailable")
	}
	resolved, err := filepath.Rel(root, full)
	if err != nil || controlSourcePath(resolved) {
		return nil, errors.New("sourceFile must be outside runtime and git control directories")
	}
	source, err := safeio.ReadRegularInRoot(t.cfg.Workspace, clean, childworkflowwire.MaxChildWorkflowSourceBytes)
	if err != nil {
		return nil, errors.New("sourceFile must be a regular workspace file of at most 1048576 bytes")
	}
	if len(source) == 0 || !utf8.Valid(source) {
		return nil, errors.New("sourceFile must contain nonempty UTF-8 Workflow DSL")
	}
	return source, nil
}

func (t *Toolset) callChildWorkflow(ctx context.Context, path, key string, body, result any) error {
	access := t.cfg.ChildWorkflows
	if access == nil || access.Validate(t.cfg.RunID) != nil {
		return errors.New("child workflow access is unavailable")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return errors.New("cannot encode child workflow request")
	}
	endpoint := strings.TrimSuffix(access.Endpoint, "/") + strings.ReplaceAll(path, "{run}", url.PathEscape(t.cfg.RunID))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return errors.New("cannot construct child workflow request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+access.BearerToken)
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	client := http.Client{Transport: t.childTransport, Timeout: childWorkflowTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("child workflow transport failed; retry with the same invocation key after checking custody")
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxChildWorkflowResponseBytes+1))
	if err != nil || len(data) > maxChildWorkflowResponseBytes {
		return errors.New("child workflow response exceeded its bound or could not be read")
	}
	expected := http.StatusOK
	if path == childworkflowwire.StartPath {
		expected = http.StatusAccepted
	}
	if response.StatusCode != expected {
		return childWorkflowHTTPError(response.StatusCode, data)
	}
	if err := json.Unmarshal(data, result); err != nil {
		return errors.New("invalid child workflow response")
	}
	return nil
}

func childWorkflowHTTPError(status int, data []byte) error {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	// Never return a remote message, body, URL or underlying transport error: any
	// could include credentials. A bounded code remains useful for retry decisions.
	code := "request_failed"
	if json.Unmarshal(data, &envelope) == nil && safeChildErrorCode(envelope.Error.Code) {
		code = envelope.Error.Code
	}
	return fmt.Errorf("child workflow request refused (HTTP %d, %s)", status, code)
}

func safeChildErrorCode(code string) bool {
	if code == "" || len(code) > 80 {
		return false
	}
	for _, r := range code {
		if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func childWorkflowArgs(name string, raw json.RawMessage) (map[string]string, error) {
	required := []string{"sourceFile"}
	if name == "start_child_workflow" {
		required = append(required, "invocationKey")
	}
	if name == "get_child_workflow" {
		required = []string{"invocationKey"}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("child workflow arguments must be an object")
	}
	values := map[string]string{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid child workflow arguments")
		}
		field, ok := key.(string)
		if !ok || !childArgumentAllowed(field, required) {
			return nil, errors.New("unknown child workflow argument")
		}
		if _, duplicate := values[field]; duplicate {
			return nil, errors.New("duplicate child workflow argument")
		}
		var value string
		if err := decoder.Decode(&value); err != nil || value == "" {
			return nil, errors.New("child workflow arguments require nonempty strings")
		}
		values[field] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, errors.New("invalid child workflow arguments")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("one child workflow argument object required")
	}
	if len(values) != len(required) {
		return nil, errors.New("missing child workflow argument")
	}
	if key, ok := values["invocationKey"]; ok && !validChildInvocationKey(key) {
		return nil, errors.New("invocationKey must be at most 256 bytes without padding or control characters")
	}
	if len(values["sourceFile"]) > 4096 {
		return nil, errors.New("sourceFile path is too long")
	}
	return values, nil
}

func childArgumentAllowed(field string, required []string) bool {
	for _, name := range required {
		if field == name {
			return true
		}
	}
	return false
}
func validChildInvocationKey(key string) bool {
	return key != "" && len(key) <= childworkflowwire.MaxChildWorkflowInvocationKeyBytes && utf8.ValidString(key) && strings.TrimSpace(key) == key && strings.IndexFunc(key, unicode.IsControl) < 0
}

func controlSourcePath(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".goobers" || part == ".git" {
			return true
		}
	}
	return false
}
