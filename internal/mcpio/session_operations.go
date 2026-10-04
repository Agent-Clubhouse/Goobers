package mcpio

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/sessioning"
)

// SessionOperationAccess is issued only for one live human-attributed turn.
// Its credential belongs in the private MCP config, never prompt or argv.
type SessionOperationAccess struct {
	Endpoint    string `json:"endpoint"`
	BearerToken string `json:"bearerToken"`
	// BacklogSources are discovery hints only; the host still authorizes each call.
	BacklogSources []string `json:"backlogSources,omitempty"`
}

func (SessionOperationAccess) String() string { return "[session operation access redacted]" }

// GoString also prevents diagnostics from exposing the bearer.
func (a SessionOperationAccess) GoString() string { return a.String() }

// Validate checks private launch configuration without echoing its secret.
func (a SessionOperationAccess) Validate(run string) error {
	if len(a.BacklogSources) > 32 {
		return errors.New("mcpio: too many session source hints")
	}
	seen := map[string]bool{}
	for _, source := range a.BacklogSources {
		if sessioning.ValidateBacklogList(sessioning.BacklogListRequest{SourceBindingID: source}) != nil || seen[source] {
			return errors.New("mcpio: invalid session source hint")
		}
		seen[source] = true
	}
	endpoint, err := url.Parse(a.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.RawPath != "" || (endpoint.Path != "" && endpoint.Path != "/") || !apiv1.ValidRunID(run) {
		return errors.New("mcpio: invalid session operation endpoint or run binding")
	}
	if !strings.HasPrefix(a.BearerToken, sessioning.OperationTokenPrefix) || len(a.BearerToken) != len(sessioning.OperationTokenPrefix)+64 || strings.IndexFunc(a.BearerToken, unicode.IsSpace) >= 0 {
		return errors.New("mcpio: invalid session operation grant")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(a.BearerToken, sessioning.OperationTokenPrefix)); err != nil {
		return errors.New("mcpio: invalid session operation grant")
	}
	return nil
}

// SessionOperationToolNames returns fresh names for complete trusted access.
func SessionOperationToolNames(a *SessionOperationAccess, run string) []string {
	if a == nil || a.Validate(run) != nil {
		return nil
	}
	return []string{"get_backlog_item", "list_backlog_items"}
}
func isSessionOperationTool(name string) bool {
	return name == "get_backlog_item" || name == "list_backlog_items"
}

func (s *Server) callSessionOperation(name string, raw json.RawMessage) (map[string]interface{}, error) {
	if len(SessionOperationToolNames(s.tools.cfg.SessionOperations, s.tools.cfg.RunID)) == 0 {
		return nil, errors.New("session operations unavailable for this invocation")
	}
	var body any
	switch name {
	case "get_backlog_item":
		request, err := sessioning.DecodeBacklogRead(raw)
		if err != nil {
			return nil, err
		}
		body = request
	case "list_backlog_items":
		request, err := sessioning.DecodeBacklogList(raw)
		if err != nil {
			return nil, err
		}
		body = request
	default:
		return nil, errors.New("unknown session operation")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("session operation request unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	access := s.tools.cfg.SessionOperations
	path := strings.ReplaceAll(sessioning.OperationPath, "{run}", url.PathEscape(s.tools.cfg.RunID)) + "/" + name
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(access.Endpoint, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("session operation transport unavailable")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+access.BearerToken)
	client := http.Client{Transport: s.tools.sessionTransport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("session operation transport failed")
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, sessioning.MaxOperationResultBytes+1))
	if err != nil || len(data) > sessioning.MaxOperationResultBytes {
		return nil, errors.New("session operation response exceeded its bound")
	}
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("session operation refused; the live human policy, source binding or operation limit may have changed")
	}
	if !json.Valid(data) {
		return nil, errors.New("session operation response was invalid")
	}
	return textResult(strings.ReplaceAll(string(data), access.BearerToken, "[REDACTED]")), nil
}
