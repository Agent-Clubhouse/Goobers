package v1alpha1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// WorkspaceRevisionInvalidCode identifies a malformed revision control.
const WorkspaceRevisionInvalidCode = "workspace_revision_invalid"

// WorkspaceRevisionDecodeError distinguishes malformed controls from transport
// failures and unrelated envelope syntax errors.
// +kubebuilder:object:generate=false
type WorkspaceRevisionDecodeError struct {
	Err error
}

func (e *WorkspaceRevisionDecodeError) Error() string { return "workspaceRevision: " + e.Err.Error() }
func (e *WorkspaceRevisionDecodeError) Unwrap() error { return e.Err }

// StageErrorCode preserves semantic classification through journal recovery.
func (e *WorkspaceRevisionDecodeError) StageErrorCode() string { return WorkspaceRevisionInvalidCode }

// DecodeWorkspaceRevisionField reads only the new control from a carrier object.
// Unrelated members retain their existing decoding and extension semantics.
func DecodeWorkspaceRevisionField(data []byte) (*WorkspaceRevision, error) {
	var syntax json.RawMessage
	if err := json.Unmarshal(data, &syntax); err != nil {
		return nil, err
	}
	if first := bytes.TrimSpace(data); len(first) == 0 || first[0] != '{' {
		return nil, nil
	}
	var revision *WorkspaceRevision
	seen := false
	err := visitRevisionObject(data, func(key string, raw json.RawMessage) error {
		if !strings.EqualFold(key, "workspaceRevision") {
			return nil
		}
		if key != "workspaceRevision" || seen {
			return &WorkspaceRevisionDecodeError{Err: fmt.Errorf("unexpected or repeated control member %q", key)}
		}
		seen = true
		var decoded WorkspaceRevision
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return err
		}
		revision = &decoded
		return nil
	})
	return revision, err
}

func visitRevisionObject(data []byte, visit func(string, json.RawMessage) error) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return fmt.Errorf("expected an object")
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("expected a member name")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if err := visit(key, raw); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func validateRevisionJSONMembers(data []byte, identity bool) error {
	fields := []string{"repository", "commitSha", "sourceRef", "sourceId", "baseRepository", "baseSha"}
	if identity {
		fields = []string{"provider", "url", "owner", "project", "name", "id"}
	}
	seen := make(map[string]bool)
	return visitRevisionObject(data, func(key string, raw json.RawMessage) error {
		if !slices.Contains(fields, key) || seen[key] {
			return fmt.Errorf("unexpected or repeated member %q", key)
		}
		seen[key] = true
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("member %q must not be null", key)
		}
		if !identity && (key == "repository" || key == "baseRepository") {
			return validateRevisionJSONMembers(raw, true)
		}
		return nil
	})
}

// UnmarshalJSON enforces the revision field without tightening legacy envelopes.
func (r *ResultEnvelope) UnmarshalJSON(data []byte) error {
	if _, err := DecodeWorkspaceRevisionField(data); err != nil {
		return err
	}
	type envelope ResultEnvelope
	return json.Unmarshal(data, (*envelope)(r))
}

// UnmarshalJSON enforces the revision field on live and recorded invocations.
func (r *InvocationEnvelope) UnmarshalJSON(data []byte) error {
	if _, err := DecodeWorkspaceRevisionField(data); err != nil {
		return err
	}
	type envelope InvocationEnvelope
	return json.Unmarshal(data, (*envelope)(r))
}
