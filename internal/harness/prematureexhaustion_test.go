package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/diagnostics/executiondeadline"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/telemetry"
)

// observedWindow simulates the runtime beginning one bounded execution with a
// 30-minute deadline and returns the window plus its observed start, so tests
// place the completion at an exact offset without reading the wall clock.
func observedWindow(t *testing.T) (*executiondeadline.Window, time.Time) {
	t.Helper()
	ctx, window := executiondeadline.Observe(context.Background(), nil, "", 1)
	bounded, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	_, done := invoke.BeginExecution(bounded)
	done()
	started, _, ok := window.Observed()
	if !ok {
		t.Fatal("precondition: no bounded execution observed")
	}
	return window, started
}

func exhaustionClaim(status apiv1.ResultStatus, code string) apiv1.ResultEnvelope {
	return apiv1.ResultEnvelope{
		Status:  status,
		Summary: "stopped before mutating",
		Error: &apiv1.ErrorInfo{
			Code:    code,
			Message: "The required complete claimed-items context was not available in time to safely perform and verify issue mutations.",
		},
	}
}

func claimedItemsPointer(size int64) []apiv1.ContextPointer {
	return []apiv1.ContextPointer{{Name: "claimed-items", Artifact: &apiv1.ArtifactPointer{Path: "artifacts/query-backlog/claimed-items.json", Digest: "sha256:00", Size: size}}}
}

func TestPrematureExhaustionClaimIsNotAnExecutionTimeout(t *testing.T) {
	window, started := observedWindow(t)
	readiness := &readinessCapture{}
	readiness.record(MCPReadiness{Server: goobersIOServerName, Category: "check_unobservable", Source: "adapter-limitation", Connection: "unobservable", Inventory: "unobservable"})
	out := Outcome{
		InputInspectionReceiptsCollected: true,
		execution:                        executionEvidence{window: window, returned: started.Add(22 * time.Second), readiness: readiness},
	}
	result := exhaustionClaim(apiv1.ResultFailure, "execution_window_exhausted")

	reclassifyPrematureExhaustionClaim(&result, claimedItemsPointer(17414), out)

	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != ErrorCodePrematureExhaustionClaim || !result.Error.Retryable {
		t.Fatalf("result = %+v, want a retryable %s failure", result, ErrorCodePrematureExhaustionClaim)
	}
	if class := telemetry.ClassifyError(result.Error.Code); class == telemetry.ErrorClassTimeout || class.InfraFault() {
		t.Fatalf("%s classifies as %q; a claim no deadline backs is neither a timeout nor infrastructure", result.Error.Code, class)
	}
	for _, want := range []string{
		"reported execution_window_exhausted after 22s of its 30m0s execution window",
		"not an execution timeout",
		"29m38s remained",
		"0 goobers-io inspection call(s) (0 failed); list_inputs not called",
		"claimed-items (17414 bytes): not inspected",
		"goobers-io readiness: check_unobservable (source adapter-limitation",
		"neither confirmed nor refuted",
		"Agent-reported cause: execution_window_exhausted: The required complete claimed-items context",
	} {
		if !strings.Contains(result.Error.Message, want) {
			t.Errorf("message missing %q:\n%s", want, result.Error.Message)
		}
	}
	_, deadline, _ := window.Observed()
	if got := result.Outputs["executionDeadline"]; got != deadline.UTC().Format(time.RFC3339) {
		t.Errorf("executionDeadline = %v, want the observed deadline %s", got, deadline.UTC().Format(time.RFC3339))
	}
	if result.Outputs["agentErrorCode"] != "execution_window_exhausted" || result.Outputs["inputsNotFullyInspected"] != "claimed-items" ||
		result.Outputs["goobersIOReadiness"] != "check_unobservable" || result.Outputs["goobersIOInspectionCalls"] != float64(0) ||
		result.Outputs["executionElapsedSeconds"] != float64(22) || result.Outputs["prematureExhaustionClaim"] != true {
		t.Errorf("outputs = %+v", result.Outputs)
	}
}

func TestPrematureExhaustionClaimReclassificationBoundaries(t *testing.T) {
	window, started := observedWindow(t)
	early := Outcome{execution: executionEvidence{window: window, returned: started.Add(time.Minute)}}
	for name, tc := range map[string]struct {
		result apiv1.ResultEnvelope
		out    Outcome
		want   string
	}{
		// A claim made past half the window is the agent's own judgment about
		// the time it had left; it keeps its code.
		"late claim": {exhaustionClaim(apiv1.ResultFailure, "EXECUTION_WINDOW_EXHAUSTED"),
			Outcome{execution: executionEvidence{window: window, returned: started.Add(16 * time.Minute)}}, "EXECUTION_WINDOW_EXHAUSTED"},
		// Without an observed bounded execution there is no deadline to judge by.
		"no deadline observed": {exhaustionClaim(apiv1.ResultFailure, "EXECUTION_WINDOW_EXHAUSTED"),
			Outcome{execution: executionEvidence{returned: started.Add(time.Second)}}, "EXECUTION_WINDOW_EXHAUSTED"},
		// An inner timeout the agent observed is a finding about the work.
		"inner timeout": {exhaustionClaim(apiv1.ResultFailure, "TEST_TIMEOUT"), early, "TEST_TIMEOUT"},
		"dependency":    {exhaustionClaim(apiv1.ResultBlocked, "DEPENDENCY_NOT_MET"), early, "DEPENDENCY_NOT_MET"},
		"runner code":   {exhaustionClaim(apiv1.ResultFailure, "timeout"), early, ErrorCodePrematureExhaustionClaim},
		"run duration":  {exhaustionClaim(apiv1.ResultFailure, "run-duration-exceeded"), early, ErrorCodePrematureExhaustionClaim},
	} {
		t.Run(name, func(t *testing.T) {
			result := tc.result
			reclassifyPrematureExhaustionClaim(&result, nil, tc.out)
			if result.Error.Code != tc.want {
				t.Fatalf("code = %q, want %q", result.Error.Code, tc.want)
			}
		})
	}

	success := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Error: &apiv1.ErrorInfo{Code: "TIMEOUT"}}
	reclassifyPrematureExhaustionClaim(&success, nil, early)
	if success.Status != apiv1.ResultSuccess || success.Error.Code != "TIMEOUT" {
		t.Fatalf("a success is never reclassified: %+v", success)
	}

	blocked := exhaustionClaim(apiv1.ResultBlocked, "EXECUTION_WINDOW_EXHAUSTED")
	reclassifyPrematureExhaustionClaim(&blocked, nil, early)
	if blocked.Status != apiv1.ResultFailure || blocked.Outputs["reclassifiedFromBlocked"] != true {
		t.Fatalf("a premature blocked claim must fail, not escalate and park its items: %+v", blocked)
	}
}

// largeClaimedItems renders a multi-item claimed-items batch pretty-printed
// past read_input's range-less line cap, so one unranged read cannot see the
// whole batch.
func largeClaimedItems(t *testing.T) []byte {
	t.Helper()
	var items []map[string]any
	for _, id := range []string{"3781", "4463", "5868"} {
		var history []string
		for i := range 900 {
			history = append(history, fmt.Sprintf("item %s history entry %d", id, i))
		}
		items = append(items, map[string]any{"id": id, "title": "item " + id, "staleness": map[string]any{"stale": false}, "history": history})
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// inspectingAdapter drives a real goobers-io server over the context the
// executor materialized, reports the readiness a Windows Copilot session
// reports, runs under an observed 30-minute deadline, and then returns an
// early exhaustion claim: the #5871 shape end to end.
type inspectingAdapter struct {
	FakeAdapter
	calls [][2]int // read_input line ranges to request; {0, 0} is a range-less read
}

func (a *inspectingAdapter) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	receiptFile := filepath.Join(".goobers", "mcp-io", mcpio.ReceiptFileName)
	if err := os.MkdirAll(filepath.Join(req.Workspace, ".goobers", "mcp-io"), 0o755); err != nil {
		return Outcome{}, err
	}
	server := mcpio.NewServer(mcpio.NewToolset(mcpio.Config{Workspace: req.Workspace, ReceiptFile: receiptFile, Inputs: req.ContextPaths}))
	var in bytes.Buffer
	line := func(id int, method string, params any) {
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		in.Write(append(data, '\n'))
	}
	line(1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if len(a.calls) > 0 {
		line(2, "tools/call", map[string]any{"name": "list_inputs"})
	}
	for i, call := range a.calls {
		args := map[string]any{"name": "claimed-items"}
		if call[0] > 0 {
			args["startLine"], args["endLine"] = call[0], call[1]
		}
		line(3+i, "tools/call", map[string]any{"name": "read_input", "arguments": args})
	}
	var stdout, stderr bytes.Buffer
	if err := server.Serve(&in, &stdout, &stderr); err != nil {
		return Outcome{}, err
	}
	if err := emitMCPReadiness(req, MCPReadiness{Server: goobersIOServerName, Category: "check_unobservable", Source: "adapter-limitation", Connection: "unobservable", Inventory: "unobservable", Authorization: "unobservable"}); err != nil {
		return Outcome{}, err
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	_, done := invoke.BeginExecution(bounded)
	defer done()
	if err := WriteCompletion(req.Workspace, req.CompletionPath, exhaustionClaim(apiv1.ResultFailure, "execution_window_exhausted")); err != nil {
		return Outcome{}, err
	}
	payload, err := readCompletion(req.Workspace, req.CompletionPath)
	if err != nil {
		return Outcome{}, err
	}
	receipts, err := mcpio.ReadInputInspectionReceipts(req.Workspace, receiptFile)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Payload: payload, InputInspectionReceipts: receipts, InputInspectionReceiptsCollected: true}, nil
}

func TestPrematureExhaustionOverLargeMultiItemBatchNamesWhatWasNotRead(t *testing.T) {
	journalRoot := t.TempDir()
	content := largeClaimedItems(t)
	ptr, err := apiv1.WriteArtifact(journalRoot, "artifacts/query-backlog/claimed-items.json", content, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	totalLines := bytes.Count(bytes.TrimRight(content, "\n"), []byte("\n")) + 1

	for name, tc := range map[string]struct {
		calls [][2]int
		want  []string
		// incomplete is outputs.inputsNotFullyInspected; nil means absent.
		incomplete any
	}{
		"never inspected": {nil, []string{"0 goobers-io inspection call(s)", "list_inputs not called", "claimed-items (" + fmt.Sprint(len(content)) + " bytes): not inspected"}, "claimed-items"},
		// One unranged read returns the first page only. The batch was NOT
		// fully inspected, and the failure has to say so with the coverage.
		"first page only": {[][2]int{{0, 0}}, []string{"2 goobers-io inspection call(s)", "list_inputs called", fmt.Sprintf("partially read (2000 of %d lines)", totalLines)}, "claimed-items"},
		"whole batch":     {[][2]int{{0, 0}, {2001, totalLines}}, []string{"3 goobers-io inspection call(s)", "claimed-items (" + fmt.Sprint(len(content)) + " bytes): fully read"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &fakeRecorder{dir: journalRoot}
			exec, err := NewExecutor(&inspectingAdapter{calls: tc.calls}, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "")
			if err != nil {
				t.Fatal(err)
			}
			env := testEnvelope(t.TempDir())
			env.TaskID = "prioritize"
			env.ContextPointers = []apiv1.ContextPointer{{Name: "claimed-items", Artifact: &ptr}}
			result, err := exec.Invoke(context.Background(), env)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != ErrorCodePrematureExhaustionClaim {
				t.Fatalf("result = %+v, want %s", result, ErrorCodePrematureExhaustionClaim)
			}
			for _, want := range append(tc.want, "not an execution timeout", "goobers-io readiness: check_unobservable") {
				if !strings.Contains(result.Error.Message, want) {
					t.Errorf("message missing %q:\n%s", want, result.Error.Message)
				}
			}
			if got := result.Outputs["inputsNotFullyInspected"]; got != tc.incomplete {
				t.Errorf("inputsNotFullyInspected = %v, want %v", got, tc.incomplete)
			}
			if len(result.Artifacts) != 0 {
				t.Errorf("a rejected claim admits no artifacts: %+v", result.Artifacts)
			}
		})
	}
}

func TestInputInspectionStateRequiresFullCoverage(t *testing.T) {
	read := func(start, end, total int) mcpio.InputInspectionReceipt {
		return mcpio.InputInspectionReceipt{Tool: "read_input", Input: "batch", Success: true, StartLine: start, EndLine: end, TotalLines: total}
	}
	for name, tc := range map[string]struct {
		receipts []mcpio.InputInspectionReceipt
		want     string
		complete bool
	}{
		"gap":          {[]mcpio.InputInspectionReceipt{read(1, 100, 300), read(201, 300, 300)}, "partially read (200 of 300 lines)", false},
		"overlap":      {[]mcpio.InputInspectionReceipt{read(1, 200, 300), read(150, 300, 300)}, "fully read", true},
		"failed read":  {[]mcpio.InputInspectionReceipt{{Tool: "read_input", Input: "batch", Success: false}}, "not inspected", false},
		"other input":  {[]mcpio.InputInspectionReceipt{{Tool: "read_input", Input: "other", Success: true, StartLine: 1, EndLine: 300, TotalLines: 300}}, "not inspected", false},
		"grep only":    {[]mcpio.InputInspectionReceipt{{Tool: "grep_input", Input: "batch", Success: true, MatchLines: []int{4}}}, "searched with grep_input but never read", false},
		"empty input":  {[]mcpio.InputInspectionReceipt{read(1, 0, 0)}, "fully read (empty input)", true},
		"no receipts":  {nil, "not inspected", false},
		"single range": {[]mcpio.InputInspectionReceipt{read(1, 2000, 2700)}, "partially read (2000 of 2700 lines)", false},
	} {
		t.Run(name, func(t *testing.T) {
			got, complete := inputInspectionState("batch", tc.receipts)
			if got != tc.want || complete != tc.complete {
				t.Fatalf("state = %q/%v, want %q/%v", got, complete, tc.want, tc.complete)
			}
		})
	}
}
