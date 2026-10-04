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
	"slices"
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
	BacklogSources        []string `json:"backlogSources,omitempty"`
	BacklogWriteSources   []string `json:"backlogWriteSources,omitempty"`
	BacklogResolveSources []string `json:"backlogResolveSources,omitempty"`
	PRRepairEnabled       bool     `json:"prRepairEnabled,omitempty"`
	BacklogReadDisabled   bool     `json:"backlogReadDisabled,omitempty"`
}

func (SessionOperationAccess) String() string { return "[session operation access redacted]" }

// GoString also prevents diagnostics from exposing the bearer.
func (a SessionOperationAccess) GoString() string { return a.String() }

// Validate checks private launch configuration without echoing its secret.
func (a SessionOperationAccess) Validate(run string) error {
	if len(a.BacklogSources) > 32 || len(a.BacklogWriteSources) > 32 || len(a.BacklogResolveSources) > 32 {
		return errors.New("mcpio: too many session source hints")
	}
	for _, sources := range [][]string{a.BacklogSources, a.BacklogWriteSources, a.BacklogResolveSources} {
		seen := map[string]bool{}
		for _, source := range sources {
			if sessioning.ValidateBacklogList(sessioning.BacklogListRequest{SourceBindingID: source}) != nil || seen[source] {
				return errors.New("mcpio: invalid session source hint")
			}
			seen[source] = true
		}
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
	var names []string
	if !a.BacklogReadDisabled {
		names = append(names, "get_backlog_item", "list_backlog_items")
	}
	if len(a.BacklogWriteSources) > 0 {
		names = append(names, "get_backlog_edit_capabilities", "edit_backlog_item", "get_backlog_edit_receipt")
	}
	if len(a.BacklogResolveSources) > 0 {
		names = append(names, "inspect_needs_human", "resolve_needs_human", "get_needs_human_receipt")
	}
	if a.PRRepairEnabled {
		names = append(names, "inspect_selected_pr", "read_selected_pr_file", "repair_selected_pr", "get_pr_repair_receipt")
	}
	return names
}
func isSessionOperationTool(name string) bool {
	switch name {
	case "inspect_selected_pr", "read_selected_pr_file", "repair_selected_pr", "get_pr_repair_receipt", "inspect_needs_human", "resolve_needs_human", "get_needs_human_receipt", "get_backlog_item", "list_backlog_items", "get_backlog_edit_capabilities", "edit_backlog_item", "get_backlog_edit_receipt":
		return true
	}
	return false
}

func (s *Server) callSessionOperation(name string, raw json.RawMessage) (map[string]interface{}, error) {
	if len(SessionOperationToolNames(s.tools.cfg.SessionOperations, s.tools.cfg.RunID)) == 0 {
		return nil, errors.New("session operations unavailable for this invocation")
	}
	if !slices.Contains(SessionOperationToolNames(s.tools.cfg.SessionOperations, s.tools.cfg.RunID), name) {
		return nil, errors.New("session operation unavailable")
	}
	if err := validateSessionOperationArguments(name, raw); err != nil {
		return nil, err
	}
	payload := []byte(raw)
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

func validateSessionOperationArguments(name string, raw json.RawMessage) error {
	var err error
	switch name {
	case "inspect_selected_pr":
		_, err = sessioning.DecodePRRepairInspect(raw)
	case "read_selected_pr_file":
		_, err = sessioning.DecodePRRepairRead(raw)
	case "repair_selected_pr":
		_, err = sessioning.DecodePRRepairRequest(raw)
	case "get_pr_repair_receipt":
		_, err = sessioning.DecodePRRepairReceipt(raw)
	case "inspect_needs_human":
		_, err = sessioning.DecodeNeedsHumanInspect(raw)
	case "resolve_needs_human":
		_, err = sessioning.DecodeNeedsHumanResolution(raw)
	case "get_needs_human_receipt":
		_, err = sessioning.DecodeNeedsHumanReceipt(raw)
	case "get_backlog_item":
		_, err = sessioning.DecodeBacklogRead(raw)
	case "list_backlog_items":
		_, err = sessioning.DecodeBacklogList(raw)
	case "get_backlog_edit_capabilities":
		_, err = sessioning.DecodeBacklogCapabilities(raw)
	case "edit_backlog_item":
		_, err = sessioning.DecodeBacklogEdit(raw)
	case "get_backlog_edit_receipt":
		_, err = sessioning.DecodeBacklogReceipt(raw)
	default:
		err = errors.New("unknown session operation")
	}
	return err
}
