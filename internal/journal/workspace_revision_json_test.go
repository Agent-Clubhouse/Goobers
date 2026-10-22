package journal

import (
	"encoding/json"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestWorkspaceRevisionConformanceDiagnostic(t *testing.T) {
	first := NormativeEvent{WorkspaceRevision: "first"}
	second := NormativeEvent{WorkspaceRevision: "second"}
	if first == second || first.String() == second.String() {
		t.Fatal("diagnostics hide a normative workspace revision difference")
	}
}

func TestWorkspaceRevisionJournalErrorPreservesSemanticCode(t *testing.T) {
	var event Event
	err := json.Unmarshal([]byte(`{"type":"stage.finished","status":"failure","workspaceRevision":null}`), &event)
	var coded interface{ StageErrorCode() string }
	if !errors.As(err, &coded) || coded.StageErrorCode() != apiv1.WorkspaceRevisionInvalidCode {
		t.Fatalf("journal decode lost the semantic code: %v", err)
	}
}
