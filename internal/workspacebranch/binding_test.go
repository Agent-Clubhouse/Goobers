package workspacebranch

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestOwnedBranchAuthorityAndClosedControl(t *testing.T) {
	base := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "base"}
	revision := &apiv1.WorkspaceRevision{CommitSHA: strings.Repeat("a", 40), SourceRef: "malicious/source/name"}
	binding, err := Expected(base, revision, "ns/", "workflow", "run")
	if err != nil || binding.Ref != "refs/heads/ns/workflow/run" {
		t.Fatalf("binding = %+v, %v", binding, err)
	}
	task := apiv1.Task{Type: apiv1.TaskDeterministic, Inputs: map[string]string{"kind": KindEstablish}}
	result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceBranchBinding: binding}
	for _, current := range []*apiv1.WorkspaceBranchBinding{nil, binding} {
		got, err := ValidateResult(current, revision, base, "ns/", "workflow", "run", task, result)
		if err != nil || got == binding || *got != *binding {
			t.Fatalf("accept = %+v, %v", got, err)
		}
	}
	task.Type = apiv1.TaskAgentic
	if _, err := ValidateResult(nil, revision, base, "ns/", "workflow", "run", task, result); err == nil {
		t.Fatal("agentic ownership accepted")
	}
	result.WorkspaceBranchBinding = nil
	result.Outputs = map[string]any{"workspaceBranch": "ns/another/run"}
	if _, err := ValidateResult(binding, revision, base, "ns/", "workflow", "run", task, result); err == nil {
		t.Fatal("scalar branch redirection accepted")
	}
	data, _ := json.Marshal(map[string]any{"workspaceBranchBinding": binding})
	if got, err := ResultBinding(data); err != nil || *got != *binding {
		t.Fatalf("decode = %+v, %v", got, err)
	}
	for _, raw := range []string{"null", "false", `"branch"`, `{"repository":{},"ref":"refs/heads/ns/w/r","startingSha":"abc","credential":"secret"}`} {
		if _, err := ResultBinding([]byte(`{"workspaceBranchBinding":` + raw + `}`)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestOwnedBranchStageCredentials(t *testing.T) {
	keys, err := StageCredentialKeys([]string{"agent:model", "repo:push", "contents:read", "repo:read"}, true)
	if err != nil || len(keys) != 1 || keys[0] != "agent:model" {
		t.Fatalf("authoring credentials = %v, %v", keys, err)
	}
	for _, key := range []string{"repo:push", "github:issues:write", "mcp:repository"} {
		if _, err := StageCredentialKeys([]string{key}, false); err == nil {
			t.Fatalf("exposed %s", key)
		}
	}
}

func TestOwnedBranchKindCannotBeOverridden(t *testing.T) {
	for _, kind := range []string{KindEstablish, KindPublish} {
		task := apiv1.Task{Type: apiv1.TaskDeterministic, Inputs: map[string]string{"kind": kind}}
		if err := ValidateKind(task, map[string]any{"kind": kind}); err != nil {
			t.Fatal(err)
		}
		if err := ValidateKind(task, map[string]any{"kind": "ordinary"}); err == nil {
			t.Fatal("backend operation replaced dynamically")
		}
		task.Inputs = nil
		if err := ValidateKind(task, map[string]any{"kind": kind}); err == nil {
			t.Fatal("backend operation introduced dynamically")
		}
		task.Inputs, task.Type = map[string]string{"kind": kind}, apiv1.TaskAgentic
		if err := ValidateKind(task, map[string]any{"kind": kind}); err == nil {
			t.Fatal("agent selected backend operation")
		}
	}
}
