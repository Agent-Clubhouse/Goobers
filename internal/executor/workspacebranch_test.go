package executor

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestOwnedBranchResultFilePromotion(t *testing.T) {
	sha := strings.Repeat("a", 40)
	data := `{"workspaceBranchBinding":{"repository":{"provider":"github","owner":"acme","name":"base"},"ref":"refs/heads/ns/workflow/run","startingSha":"` + sha + `"},"workspaceBranch":"ns/workflow/run","workspaceBranchTip":"` + sha + `"}`
	result := apiv1.ResultEnvelope{}
	if err := mergeResultFileOutputs(&result, []byte(data)); err != nil {
		t.Fatal(err)
	}
	if result.WorkspaceBranchBinding == nil || result.WorkspaceBranchBinding.StartingSHA != sha ||
		result.Outputs["workspaceBranch"] != "ns/workflow/run" || result.WorkspaceBranchTip != sha {
		t.Fatal("typed ownership or scalar continuity was not promoted")
	}
	if _, exists := result.Outputs["workspaceBranchBinding"]; exists {
		t.Fatal("typed ownership escaped into scalar outputs")
	}
	if _, exists := result.Outputs["workspaceBranchTip"]; exists {
		t.Fatal("typed publication tip escaped into scalar outputs")
	}
	for _, raw := range []string{"null", `"main"`, `{"unknown":true}`} {
		if err := mergeResultFileOutputs(&apiv1.ResultEnvelope{}, []byte(`{"workspaceBranchBinding":`+raw+`}`)); err == nil {
			t.Fatalf("invalid ownership accepted: %s", raw)
		}
		if err := mergeResultFileOutputs(&apiv1.ResultEnvelope{}, []byte(`{"workspaceBranchTip":`+raw+`}`)); err == nil {
			t.Fatalf("invalid publication tip accepted: %s", raw)
		}
	}
}
