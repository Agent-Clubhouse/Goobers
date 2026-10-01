package journal

import (
	"errors"
	"fmt"
	"testing"
)

type typedCause struct {
	code  string
	class string
	err   error
}

func (e typedCause) Error() string { return e.err.Error() }

func (e typedCause) Unwrap() error { return e.err }

func (e typedCause) ErrorCode() string { return e.code }

func (e typedCause) ErrorClass() string { return e.class }

func TestErrorDetailForPreservesWrappedCauseBoundaries(t *testing.T) {
	leaf := errors.New(`a rebound branch requires C:\repo:work and https://example.test/a:b`)
	workspace := fmt.Errorf("create read-only workspace: %w", leaf)
	err := fmt.Errorf(`runner: prepare gate "review": %w`, workspace)

	detail := ErrorDetailFor("run_failed", err)
	if detail == nil {
		t.Fatal("ErrorDetailFor returned nil")
	}
	if detail.Code != "run_failed" || detail.Message != err.Error() {
		t.Fatalf("detail = %+v, want code run_failed and complete message %q", detail, err.Error())
	}
	got := causeMessages(detail.Causes)
	want := []string{
		`runner: prepare gate "review"`,
		"create read-only workspace",
		`a rebound branch requires C:\repo:work and https://example.test/a:b`,
	}
	if !equalStrings(got, want) {
		t.Fatalf("cause messages = %#v, want %#v", got, want)
	}
}

func TestErrorCausesRetainsTypedCodeOnOwningLayerOnly(t *testing.T) {
	leaf := typedCause{code: "workspace_failed", class: "infra", err: errors.New("workspace unavailable")}
	err := fmt.Errorf("runner: dispatch: %w", leaf)

	causes := ErrorCauses(err)
	if len(causes) != 2 {
		t.Fatalf("len(causes) = %d, want 2: %#v", len(causes), causes)
	}
	if causes[0].Code != "" || causes[0].Class != "" {
		t.Fatalf("outer cause inherited typed metadata: %+v", causes[0])
	}
	if causes[1].Code != "workspace_failed" || causes[1].Class != "infra" {
		t.Fatalf("leaf typed metadata = %+v, want code/class", causes[1])
	}
}

func TestErrorCausesContinueThroughTypedWrapperAroundWrappedChain(t *testing.T) {
	leaf := errors.New("leaf unavailable")
	wrapped := fmt.Errorf("inner boundary: %w", leaf)
	err := fmt.Errorf("outer boundary: %w", typedCause{code: "typed_boundary", class: "infra", err: wrapped})

	causes := ErrorCauses(err)
	got := causeMessages(causes)
	want := []string{"outer boundary", "inner boundary: leaf unavailable", "inner boundary", "leaf unavailable"}
	if !equalStrings(got, want) {
		t.Fatalf("cause messages = %#v, want %#v (full causes %#v)", got, want, causes)
	}
	if causes[1].Code != "typed_boundary" || causes[1].Class != "infra" {
		t.Fatalf("typed wrapper metadata = %+v, want code/class", causes[1])
	}
}

func TestErrorInfoForPreservesTypedCauseMetadata(t *testing.T) {
	err := fmt.Errorf("stage result: %w", typedCause{code: "provider_error", class: "infra", err: errors.New("provider unavailable")})

	info := ErrorInfoFor("stage_failed", err, true)
	if info == nil {
		t.Fatal("ErrorInfoFor returned nil")
	}
	if info.Code != "stage_failed" || info.Message != err.Error() || !info.Retryable {
		t.Fatalf("info = %+v, want code/message/retryable", info)
	}
	got := make([]string, len(info.Causes))
	for i, cause := range info.Causes {
		got[i] = cause.Message
	}
	if !equalStrings(got, []string{"stage result", "provider unavailable"}) {
		t.Fatalf("cause messages = %#v, want structured chain", got)
	}
	if info.Causes[1].Code != "provider_error" || info.Causes[1].Class != "infra" {
		t.Fatalf("typed cause metadata = %+v, want code/class", info.Causes[1])
	}
}

func causeMessages(causes []ErrorCause) []string {
	out := make([]string, len(causes))
	for i, cause := range causes {
		out[i] = cause.Message
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
