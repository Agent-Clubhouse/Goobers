package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// liveDenialTranscript reproduces the exact runtime line observed on the
// affected instance, where the CLI refused a tool call and the agent went on
// to classify the run as organization content exclusion.
const liveDenialTranscript = `{"type":"tool_result","name":"powershell","content":"Permission denied and could not request permission from user"}`

// TestReclassifyToolPermissionBlockConvertsDenialToDistinctFailure is #2962's
// core regression: a denied tool call reported by the agent as content
// exclusion must surface as a tool-permission failure, not a block that parks
// the driving issue for a human who cannot fix it.
func TestReclassifyToolPermissionBlockConvertsDenialToDistinctFailure(t *testing.T) {
	result := apiv1.ResultEnvelope{
		Status:  apiv1.ResultBlocked,
		Summary: "cannot proceed: organization content exclusion policy blocks these files",
		Error: &apiv1.ErrorInfo{
			Code:    "CONTENT_EXCLUSION",
			Message: "the org content exclusion policy prevented reading the workspace",
		},
		Outputs: map[string]interface{}{"blockedBy": "content-exclusion-policy"},
	}

	reclassifyToolPermissionBlock(&result, []byte(liveDenialTranscript), nil)

	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %q, want %q — a denied tool is a fault, not a dependency block", result.Status, apiv1.ResultFailure)
	}
	if result.Error == nil || result.Error.Code != ErrorCodeToolPermissionDenied {
		t.Fatalf("error = %+v, want code %s", result.Error, ErrorCodeToolPermissionDenied)
	}
	if result.Error.Retryable {
		t.Error("retryable = true, want false — the identical invocation reproduces the identical refusal")
	}
	if !strings.Contains(result.Error.Message, "Permission denied and could not request permission from user") {
		t.Errorf("message omitted the runtime evidence: %q", result.Error.Message)
	}
	if !strings.Contains(result.Error.Message, "CONTENT_EXCLUSION") {
		t.Errorf("message discarded the agent's own account: %q", result.Error.Message)
	}
	if result.Outputs["toolPermissionDenied"] != true {
		t.Errorf("outputs.toolPermissionDenied = %v, want true", result.Outputs["toolPermissionDenied"])
	}
	if result.Outputs["contentExclusionClaimRejected"] != true {
		t.Errorf("outputs.contentExclusionClaimRejected = %v, want true", result.Outputs["contentExclusionClaimRejected"])
	}
}

// TestReclassifyToolPermissionBlockRejectsUnsubstantiatedClaim proves a
// content-exclusion classification with no runtime signal at all is refused:
// content exclusion is an organization policy fact, not something a model may
// infer from a tool call it could not make.
func TestReclassifyToolPermissionBlockRejectsUnsubstantiatedClaim(t *testing.T) {
	result := apiv1.ResultEnvelope{
		Status:  apiv1.ResultBlocked,
		Summary: "blocked by content exclusion",
		Error:   &apiv1.ErrorInfo{Code: "BLOCKED", Message: "cannot read the files"},
	}

	reclassifyToolPermissionBlock(&result, []byte(`{"type":"assistant","content":"I will try another approach."}`), nil)

	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %q, want %q", result.Status, apiv1.ResultFailure)
	}
	if result.Error == nil || result.Error.Code != ErrorCodeUnsubstantiatedContentExclusion {
		t.Fatalf("error = %+v, want code %s", result.Error, ErrorCodeUnsubstantiatedContentExclusion)
	}
	if result.Outputs["toolPermissionDenied"] != false {
		t.Errorf("outputs.toolPermissionDenied = %v, want false", result.Outputs["toolPermissionDenied"])
	}
}

// TestReclassifyToolPermissionBlockPreservesSignalledExclusion is the guard
// against overreach: when the runtime itself reports content exclusion, the
// classification is substantiated and must survive untouched.
func TestReclassifyToolPermissionBlockPreservesSignalledExclusion(t *testing.T) {
	transcript := `{"type":"tool_result","content":"This file is excluded by your organization's content exclusion rules."}`
	result := apiv1.ResultEnvelope{
		Status:  apiv1.ResultBlocked,
		Summary: "content exclusion prevents review",
		Error:   &apiv1.ErrorInfo{Code: "CONTENT_EXCLUSION", Message: "excluded paths"},
	}
	before := result

	reclassifyToolPermissionBlock(&result, []byte(transcript), nil)

	if result.Status != apiv1.ResultBlocked {
		t.Fatalf("status = %q, want %q — an explicitly signalled exclusion is real", result.Status, apiv1.ResultBlocked)
	}
	if result.Summary != before.Summary || result.Error.Code != before.Error.Code {
		t.Fatalf("result was rewritten: %+v", result)
	}
	if result.Outputs != nil {
		t.Fatalf("outputs = %+v, want untouched", result.Outputs)
	}
}

// TestReclassifyToolPermissionBlockLeavesOrdinaryBlocksAlone proves the guard
// is scoped to content-exclusion claims: the documented dependency-block path
// must be completely unaffected, even when a tool refusal also appears.
func TestReclassifyToolPermissionBlockLeavesOrdinaryBlocksAlone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		result     apiv1.ResultEnvelope
		transcript string
	}{
		{
			name: "dependency block with a denial in the transcript",
			result: apiv1.ResultEnvelope{
				Status:  apiv1.ResultBlocked,
				Summary: "waiting on the schema change",
				Error:   &apiv1.ErrorInfo{Code: "DEPENDENCY_NOT_MET", Message: "needs #512"},
				Outputs: map[string]interface{}{"blockedBy": "512"},
			},
			transcript: liveDenialTranscript,
		},
		{
			name:       "successful result mentioning content exclusion",
			result:     apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "documented the content exclusion policy"},
			transcript: liveDenialTranscript,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.result
			reclassifyToolPermissionBlock(&got, []byte(tc.transcript), nil)
			if got.Status != tc.result.Status {
				t.Fatalf("status = %q, want %q", got.Status, tc.result.Status)
			}
			if got.Error != nil && tc.result.Error != nil && got.Error.Code != tc.result.Error.Code {
				t.Fatalf("error code = %q, want %q", got.Error.Code, tc.result.Error.Code)
			}
		})
	}
}

// TestReclassifyToolPermissionBlockReadsStderr proves the evidence scan covers
// stderr too: the CLI does not always route a refusal through the transcript.
func TestReclassifyToolPermissionBlockReadsStderr(t *testing.T) {
	result := apiv1.ResultEnvelope{
		Status:  apiv1.ResultBlocked,
		Summary: "org content policy blocks this",
	}
	reclassifyToolPermissionBlock(&result, nil, []byte("Permission denied and could not request permission from user\n"))
	if result.Error == nil || result.Error.Code != ErrorCodeToolPermissionDenied {
		t.Fatalf("error = %+v, want code %s from stderr evidence", result.Error, ErrorCodeToolPermissionDenied)
	}
}

// TestObserveToolPermissionsBoundsQuotes keeps a pathological transcript from
// inflating the result envelope: evidence is de-duplicated and capped.
func TestObserveToolPermissionsBoundsQuotes(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("Permission denied and could not request permission from user\n")
	}
	b.WriteString("tool permission denied for view\n")
	b.WriteString("tool permission denied for shell\n")
	b.WriteString("tool permission denied for edit\n")

	ev := observeToolPermissions([]byte(b.String()))
	if !ev.denied {
		t.Fatal("denied = false, want true")
	}
	if len(ev.quotes) > maxPermissionQuotes {
		t.Fatalf("quotes = %d, want at most %d", len(ev.quotes), maxPermissionQuotes)
	}
	if len(ev.quotes) != 3 {
		t.Fatalf("quotes = %v, want 3 distinct excerpts", ev.quotes)
	}
}

// TestClaimsContentExclusionScansScalarOutputs covers the live shape where the
// claim arrived only through outputs.blockedBy as free text, violating the
// documented comma-separated-numbers contract.
func TestClaimsContentExclusionScansScalarOutputs(t *testing.T) {
	if !claimsContentExclusion(apiv1.ResultEnvelope{
		Outputs: map[string]interface{}{"blockedBy": "content-exclusion-policy"},
	}) {
		t.Error("claimsContentExclusion = false for a content-exclusion blockedBy output")
	}
	if claimsContentExclusion(apiv1.ResultEnvelope{
		Summary: "implemented the retry policy",
		Outputs: map[string]interface{}{"blockedBy": "441", "attempts": 2},
	}) {
		t.Error("claimsContentExclusion = true for an ordinary result")
	}
}

// TestWriteCopilotInvocationDiagnostics covers #2962's fourth suggested fix:
// the CLI version and the effective tool/permission arguments are recorded, so
// a later refusal can be attributed. The prompt must never appear there.
func TestWriteCopilotInvocationDiagnostics(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{
		Workspace:      workspace,
		HarnessVersion: "1.0.76-2",
		Tools:          []string{"view", "shell"},
	}
	argv := []string{
		"copilot", "-p", "SECRET PROMPT TEXT that must not be recorded",
		"--model", "gpt-5", "--allow-all-tools", "--log-level", "all",
		"--available-tools=view,shell", "--add-github-mcp-toolset=issues", "--silent",
	}

	if err := writeCopilotInvocationDiagnostics(req, argv); err != nil {
		t.Fatalf("writeCopilotInvocationDiagnostics: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(CopilotInvocationDiagnosticsFile)))
	if err != nil {
		t.Fatalf("read diagnostics: %v", err)
	}
	if strings.Contains(string(raw), "SECRET PROMPT TEXT") {
		t.Fatalf("diagnostics leaked the prompt: %s", raw)
	}
	var got copilotInvocationDiagnostics
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode diagnostics: %v", err)
	}
	if got.CLIVersion != "1.0.76-2" {
		t.Errorf("cliVersion = %q, want 1.0.76-2", got.CLIVersion)
	}
	if !got.ToolConstrained {
		t.Error("toolConstrained = false, want true for a declared allowlist")
	}
	want := []string{"--allow-all-tools", "--available-tools=view,shell", "--add-github-mcp-toolset=issues"}
	if strings.Join(got.PermissionArgs, " ") != strings.Join(want, " ") {
		t.Errorf("permissionArgs = %v, want %v", got.PermissionArgs, want)
	}
	for _, unwanted := range []string{"--model", "--log-level", "--silent", "-p"} {
		for _, arg := range got.PermissionArgs {
			if strings.HasPrefix(arg, unwanted) {
				t.Errorf("permissionArgs kept a non-permission flag %q", arg)
			}
		}
	}
}
