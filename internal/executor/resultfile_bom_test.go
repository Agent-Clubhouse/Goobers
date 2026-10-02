package executor

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// #5175: a result file written with a UTF-8 byte-order mark (Windows
// PowerShell 5.1's `Set-Content -Encoding utf8`) was silently treated as
// non-JSON and its declared outputs dropped. These are the exact bytes a
// production instance recorded: BOM, flat JSON, CRLF.
func TestMergeResultFileOutputsAcceptsUTF8BOM(t *testing.T) {
	data := []byte("\xEF\xBB\xBF{\"prNumber\":\"194\"}\r\n")
	result := apiv1.ResultEnvelope{}
	if err := MergeResultFileOutputs(&result, data); err != nil {
		t.Fatalf("MergeResultFileOutputs: %v", err)
	}
	if result.Outputs["prNumber"] != "194" {
		t.Fatalf("outputs = %+v, want prNumber=194 from the BOM-prefixed file", result.Outputs)
	}
}

// The BOM is stripped before the workspaceRevision control is decoded too, so
// a BOM-prefixed file cannot slip an invalid control past strict validation
// nor lose a valid one.
func TestMergeResultFileOutputsBOMKeepsWorkspaceRevisionStrict(t *testing.T) {
	sha := strings.Repeat("a", 40)
	valid := "\xEF\xBB\xBF" + `{"workspaceRevision":{"repository":{"provider":"github","owner":"org","name":"repo"},"commitSha":"` + sha + `"}}`
	result := apiv1.ResultEnvelope{}
	if err := MergeResultFileOutputs(&result, []byte(valid)); err != nil {
		t.Fatalf("valid control: %v", err)
	}
	if result.WorkspaceRevision == nil || result.WorkspaceRevision.CommitSHA != sha {
		t.Fatalf("revision = %+v, want commit %s", result.WorkspaceRevision, sha)
	}
	invalid := "\xEF\xBB\xBF" + `{"workspaceRevision":42}`
	if err := MergeResultFileOutputs(&apiv1.ResultEnvelope{}, []byte(invalid)); err == nil {
		t.Fatal("invalid control behind a BOM was accepted")
	}
}

// Only a LEADING mark is a byte-order mark; one anywhere else stays content,
// so the file remains invalid JSON and keeps the legacy no-op behaviour.
func TestMergeResultFileOutputsOnlyStripsLeadingBOM(t *testing.T) {
	for _, data := range []string{
		" \xEF\xBB\xBF{\"k\":\"v\"}",
		"\xEF\xBB\xBF\xEF\xBB\xBF{\"k\":\"v\"}",
	} {
		result := apiv1.ResultEnvelope{}
		if err := MergeResultFileOutputs(&result, []byte(data)); err != nil {
			t.Fatalf("%q: %v", data, err)
		}
		if len(result.Outputs) != 0 {
			t.Fatalf("%q: outputs = %+v, want none", data, result.Outputs)
		}
	}
}

// End to end through the shell executor: a stage that writes its declared
// result file with a BOM surfaces its outputs for a downstream inputsFrom.
func TestShellExecutor_BOMPrefixedResultFileOutputs(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "result.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `printf '\357\273\277{"prNumber":"194"}\r\n' > result.json`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v (%+v), want success", result.Status, result.Error)
	}
	if result.Outputs["prNumber"] != "194" {
		t.Fatalf("outputs = %+v, want prNumber=194", result.Outputs)
	}
}
