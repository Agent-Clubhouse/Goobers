package startfence

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
)

// startOwnerDigestGolden is InputDigest of goldenRunInput as computed by main
// before engine.childWorkflows existed (#8085 d). Every in-flight run's
// start-owner memo carries a digest computed this way; a field added to
// RunInput that changes the encoding of an input not using it (a missing
// omitempty, a renamed key) would make a rolling deploy read every in-flight
// run as another owner's (ErrNotMine). Never update this value to make the
// test pass: an encoding change for existing inputs needs its own migration.
const startOwnerDigestGolden = "sha256:6d05a17a99d5891af14643d780e27b0329143345fe8c99afc890f6a7257daf85"

func goldenRunInput() engine.RunInput {
	return engine.RunInput{
		RunID: "run-golden", AcceptanceID: "trigger-golden", WorkflowName: "implementation", Version: 3, Gaggle: "web",
		DSLVersion: "3.1", WorkflowDigest: "sha256:workflow", GooberDigest: "sha256:goober", ConfigGeneration: "sha256:generation",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "owner", Name: "repo"}, BranchNamespace: "goobers",
		LiveJournal: true, OwnerEpoch: 7, StarterID: "control-1",
		Spec: apiv1.WorkflowSpec{Gaggle: "web", Start: "build", Tasks: []apiv1.Task{{Name: "build", Type: apiv1.TaskAgentic, Goal: "build it", Goober: "coder"}}},
	}
}

func TestStartOwnerDigestOfAnInputWithoutChildWorkflowsIsUnchanged(t *testing.T) {
	in := goldenRunInput()
	digest, err := InputDigest(in)
	if err != nil || digest != startOwnerDigestGolden {
		t.Fatalf("start-owner digest = %s (%v), want the pre-switch %s: in-flight runs would read as another owner's", digest, err, startOwnerDigestGolden)
	}
	in.ChildWorkflowsEnabled = true
	if opted, err := InputDigest(in); err != nil || opted == startOwnerDigestGolden {
		t.Fatalf("an opted-in input digests as %s (%v), want it pinned into the digest", opted, err)
	}
}
