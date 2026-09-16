package executor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func TestResultFileWorkspaceRevisionPromotion(t *testing.T) {
	revision := apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		CommitSHA:  strings.Repeat("a", 40), SourceRef: "feature",
	}
	data, err := json.Marshal(map[string]any{"workspaceRevision": revision, "prNumber": "42", "context": map[string]any{"ignored": true}})
	if err != nil {
		t.Fatal(err)
	}
	var result apiv1.ResultEnvelope
	if err := mergeResultFileOutputs(&result, data); err != nil {
		t.Fatal(err)
	}
	if result.WorkspaceRevision == nil || result.WorkspaceRevision.CommitSHA != revision.CommitSHA || result.WorkspaceRevision.Repository != revision.Repository {
		t.Fatalf("typed control lost: %+v", result)
	}
	if len(result.Outputs) != 1 || result.Outputs["prNumber"] != "42" {
		t.Fatalf("control leaked into scalar outputs: %+v", result.Outputs)
	}
}

func TestResultFileWorkspaceRevisionRejectsMalformedControl(t *testing.T) {
	for _, raw := range []string{
		`null`, `"main"`, `false`, `{}`,
		`{"repository":{"provider":"github","owner":"acme","name":"web","connectionRef":"elevated"},"commitSha":"` + strings.Repeat("a", 40) + `"}`,
		`{"repository":{"provider":"github","owner":"acme","name":"web"},"commitSha":"main"}`,
		`{"repository":{"provider":"github","owner":"acme","name":"web"},"commitSha":"` + strings.Repeat("a", 40) + `","credential":"elevated"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var result apiv1.ResultEnvelope
			err := mergeResultFileOutputs(&result, []byte(`{"workspaceRevision":`+raw+`}`))
			var typed *workspacerevision.Error
			if !errors.As(err, &typed) || typed.Code != workspacerevision.CodeInvalid {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestResultFileWorkspaceRevisionLegacyCompatibility(t *testing.T) {
	for _, data := range []string{"plain text", `["array"]`, `{"prNumber":"42","nested":{"field":"value"}}`} {
		var result apiv1.ResultEnvelope
		if err := mergeResultFileOutputs(&result, []byte(data)); err != nil || result.WorkspaceRevision != nil {
			t.Fatalf("legacy result changed: %+v, %v", result, err)
		}
	}
}
