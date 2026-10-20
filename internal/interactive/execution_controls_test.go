package interactive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutionModePinnedAcrossTurnsRestartAndDefaultChange(t *testing.T) {
	e := newExecutor()
	opts := options(t, e)
	opts.AvailableExecutionModes = []string{ExecutionModeLocalTools, ExecutionModeCodexLocal}
	opts.DefaultExecutionMode = ExecutionModeCodexLocal
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Create("alice", CreateRequest{CommandID: "mode-pinned", SurfaceID: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if session.ExecutionMode != ExecutionModeCodexLocal {
		t.Fatal("default selection not persisted")
	}
	// The caller cannot mutate the host's adapter allowlist after Open.
	opts.AvailableExecutionModes[1] = "unknown"
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "investigate"})
	run := receiveRun(t, e)
	if run.req.ExecutionMode != ExecutionModeCodexLocal {
		t.Fatal("executor mode not pinned")
	}
	run.result <- ExecutionResult{}
	waitStatus(t, s, "alice", session.ID, "idle")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	opts.AvailableExecutionModes = []string{ExecutionModeLocalTools, ExecutionModeCodexLocal}
	opts.DefaultExecutionMode = ExecutionModeLocalTools
	restarted := openTest(t, opts)
	replayed, err := restarted.Create("alice", CreateRequest{CommandID: "mode-pinned", SurfaceID: "work"})
	if err != nil || replayed.ID != session.ID || replayed.ExecutionMode != ExecutionModeCodexLocal {
		t.Fatal("create replay followed changed host default", err)
	}
	commandTest(t, restarted, "alice", session.ID, Command{Type: "message", Text: "again"})
	again := receiveRun(t, e)
	if again.req.ExecutionMode != ExecutionModeCodexLocal {
		t.Fatal("restart silently changed adapter")
	}
	again.result <- ExecutionResult{}
	waitStatus(t, restarted, "alice", session.ID, "idle")
}

func TestUnavailableDefaultDoesNotFallBackToDemo(t *testing.T) {
	opts := options(t, newExecutor())
	opts.AvailableExecutionModes = []string{ExecutionModeLocalTools}
	opts.DefaultExecutionMode = ExecutionModeCodexLocal
	s := openTest(t, opts)
	_, err := s.Create("alice", CreateRequest{CommandID: "default-unavailable", SurfaceID: "work"})
	requireCode(t, err, "execution_mode_unavailable")
	_, err = s.Create("alice", CreateRequest{CommandID: "explicit-unavailable", SurfaceID: "work", ExecutionMode: ExecutionModeCodexLocal})
	requireCode(t, err, "execution_mode_unavailable")
	_, err = s.Create("alice", CreateRequest{CommandID: "unknown", SurfaceID: "work", ExecutionMode: "cloud-anything"})
	requireCode(t, err, "execution_mode_unavailable")
	session, err := s.Create("alice", CreateRequest{CommandID: "explicit-demo", SurfaceID: "work", ExecutionMode: ExecutionModeLocalTools})
	if err != nil || session.ExecutionMode != ExecutionModeLocalTools {
		t.Fatal("explicit demo choice refused", err)
	}
	response := apiRequest(s, http.MethodGet, apiPrefix+"/surfaces", "alice", "")
	var discovery struct {
		ExecutionModes       []string `json:"executionModes"`
		DefaultExecutionMode string   `json:"defaultExecutionMode"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &discovery); err != nil {
		t.Fatal(err)
	}
	if len(discovery.ExecutionModes) != 1 || discovery.ExecutionModes[0] != ExecutionModeLocalTools || discovery.DefaultExecutionMode != ExecutionModeCodexLocal {
		t.Fatal("discovery concealed unavailable default", response.Body.String())
	}
}

func TestUnavailableOrUnpinnedHistoricalModesAreInspectOnly(t *testing.T) {
	for _, mode := range []string{ExecutionModeCodexLocal, ""} {
		t.Run(mode, func(t *testing.T) {
			opts := options(t, newExecutor())
			opts.AvailableExecutionModes = []string{ExecutionModeLocalTools, ExecutionModeCodexLocal}
			s, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			session := createTest(t, s, "alice")
			// Model a previous file format or removal of a once-available native mode.
			s.mu.Lock()
			err = s.mutate(func(state *diskState) error { state.Sessions[session.ID].Session.ExecutionMode = mode; return nil })
			s.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			opts.AvailableExecutionModes = []string{ExecutionModeLocalTools}
			next := openTest(t, opts)
			current, err := next.Session("alice", session.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = next.Command(context.Background(), "alice", session.ID, Command{CommandID: "no-fallback", Type: "message", Text: "edit", ExpectedSequence: current.Sequence, ExpectedFence: current.Fence})
			requireCode(t, err, "execution_mode_unavailable")
			requireCode(t, next.CheckControl("alice", session.ID), "execution_mode_unavailable")
			if got := next.ListSessions("alice"); len(got) != 1 || got[0].ExecutionMode != mode {
				t.Fatal("historical identity was rewritten")
			}
			commandTest(t, next, "alice", session.ID, Command{Type: "stop"})
		})
	}
}

func TestExecutionChildProposalIsAtomicReplayableAndExactlyReviewed(t *testing.T) {
	e := newExecutor()
	opts := options(t, e)
	opts.AvailableExecutionModes = []string{ExecutionModeLocalTools, ExecutionModeCodexLocal}
	opts.DefaultExecutionMode = ExecutionModeCodexLocal
	s := openTest(t, opts)
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "plan a review"})
	parent := receiveRun(t, e)
	yaml := "# exact user-visible definition\nname: review\nsteps:\n  - inspect: src/main.go\n"
	request := func(key, prompt, source string) (CommandReceipt, error) {
		return s.RequestChildFromExecution(context.Background(), "alice", session.ID, parent.req.TurnID, parent.req.LeaseID, parent.req.Fence, key, prompt, source)
	}
	first, err := request("tool-call-1", "Review the parser", yaml)
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.emit(ExecutionEvent{Type: "assistant.message", Data: map[string]any{"text": "proposal ready"}}); err != nil {
		t.Fatal(err)
	}
	replay, err := request("tool-call-1", "Review the parser", yaml)
	if err != nil || first != replay {
		t.Fatal("host proposal replay duplicated or changed receipt", err)
	}
	_, err = request("tool-call-1", "Different prompt", yaml)
	requireCode(t, err, "idempotency_conflict")
	_, err = request("tool-call-1", "Review the parser", yaml+"# changed\n")
	requireCode(t, err, "idempotency_conflict")
	current, _ := s.Session("alice", session.ID)
	if len(current.Children) != 1 {
		t.Fatal("replay consumed more child budget")
	}
	child := current.Children[0]
	sum := sha256.Sum256([]byte(yaml))
	if child.WorkflowYAML != yaml || child.WorkflowDigest != hex.EncodeToString(sum[:]) || child.ActorSource != "execution" || child.SourceTurnID != parent.req.TurnID || child.ExecutionKind != "interactive-workflow" {
		t.Fatalf("proposal lost immutable definition/provenance: %+v", child)
	}
	if child.Status != "proposed" {
		t.Fatal("host proposal auto-approved child")
	}
	events, _ := s.Events("alice", session.ID, 0)
	for _, event := range events {
		if event.CommandID == first.CommandID && event.ActorSource != "execution" {
			t.Fatal("host command was attributed to owner input")
		}
	}
	_, err = s.Command(context.Background(), "alice", session.ID, Command{CommandID: "premature-review", Type: "child.review", ChildID: child.ID, Decision: "approve", ExpectedSequence: current.Sequence, ExpectedFence: current.Fence})
	requireCode(t, err, "checkpoint_required")
	parent.result <- ExecutionResult{}
	waitStatus(t, s, "alice", session.ID, "idle")
	commandTest(t, s, "alice", session.ID, Command{Type: "child.review", ChildID: child.ID, Decision: "approve"})
	review := receiveRun(t, e)
	if review.req.ExecutionMode != ExecutionModeCodexLocal || review.req.WorkflowYAML != yaml || !review.req.ReadOnly {
		t.Fatal("approved executor request changed mode, definition, or authority")
	}
	_, err = s.RequestChildFromExecution(context.Background(), "alice", session.ID, review.req.TurnID, review.req.LeaseID, review.req.Fence, "recursive", "expand", yaml)
	requireCode(t, err, "child_depth_denied")
	review.result <- ExecutionResult{}
	<-review.done
}

func TestExecutionChildProposalRefusesWrongIdentityLeaseFenceAndExpiredAttempt(t *testing.T) {
	e := newExecutor()
	s := openTest(t, options(t, e))
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "plan"})
	run := receiveRun(t, e)
	call := func(owner, turn, lease string, fence uint64) (CommandReceipt, error) {
		return s.RequestChildFromExecution(context.Background(), owner, session.ID, turn, lease, fence, "key", "review", "name: review\n")
	}
	_, err := call("bob", run.req.TurnID, run.req.LeaseID, run.req.Fence)
	requireCode(t, err, "not_found")
	_, err = call("alice", "wrong-turn", run.req.LeaseID, run.req.Fence)
	requireCode(t, err, "stale_lease")
	_, err = call("alice", run.req.TurnID, "wrong-lease", run.req.Fence)
	requireCode(t, err, "stale_lease")
	_, err = call("alice", run.req.TurnID, run.req.LeaseID, run.req.Fence+1)
	requireCode(t, err, "stale_fence")
	commandTest(t, s, "alice", session.ID, Command{Type: "stop"})
	waitStatus(t, s, "alice", session.ID, "stopped")
	_, err = call("alice", run.req.TurnID, run.req.LeaseID, run.req.Fence)
	requireCode(t, err, "stale_fence")
	final, _ := s.Session("alice", session.ID)
	if len(final.Children) != 0 {
		t.Fatal("untrusted host proposal escaped checks")
	}
}

func TestChildWorkflowDefinitionBoundedImmutableAndNeverScrubbedIntoDifferentApproval(t *testing.T) {
	s := openTest(t, options(t, newExecutor()))
	session := createTest(t, s, "alice")
	call := func(id, yaml string) (CommandReceipt, error) {
		current, _ := s.Session("alice", session.ID)
		return s.Command(context.Background(), "alice", session.ID, Command{CommandID: id, Type: "child.request", Text: "review", WorkflowYAML: yaml, ExpectedSequence: current.Sequence, ExpectedFence: current.Fence})
	}
	_, err := call("oversize", strings.Repeat("x", (32<<10)+1))
	requireCode(t, err, "invalid_workflow")
	secret := "ghp_" + strings.Repeat("z", 36)
	_, err = call("secret", "token: "+secret)
	requireCode(t, err, "secret_in_workflow")
	proposed, err := call("valid", "# retained bytes\nname: review\n")
	if err != nil {
		t.Fatal(err)
	}
	current, _ := s.Session("alice", session.ID)
	_, err = s.Command(context.Background(), "alice", session.ID, Command{CommandID: "mutate-at-review", Type: "child.review", ChildID: proposed.ChildID, Decision: "approve", WorkflowYAML: "name: widened\n", ExpectedSequence: current.Sequence, ExpectedFence: current.Fence})
	requireCode(t, err, "invalid_workflow")
	bytes, _ := os.ReadFile(filepath.Join(s.opts.StateDir, "sessions.json"))
	if strings.Contains(string(bytes), secret) {
		t.Fatal("credential-shaped workflow material persisted")
	}
}

func TestHostProposalReplaySurvivesParentAttemptReplacement(t *testing.T) {
	e := newExecutor()
	s := openTest(t, options(t, e))
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "design review"})
	firstRun := receiveRun(t, e)
	first, err := s.RequestChildFromExecution(context.Background(), "alice", session.ID, firstRun.req.TurnID, firstRun.req.LeaseID, firstRun.req.Fence, "logical-tool-call", "review", "name: review\n")
	if err != nil {
		t.Fatal(err)
	}
	firstRun.result <- ExecutionResult{WaitingAuth: true, AuthReason: "reauthenticate"}
	waitStatus(t, s, "alice", session.ID, "waiting_auth")
	commandTest(t, s, "alice", session.ID, Command{Type: "resume"})
	replacement := receiveRun(t, e)
	if replacement.req.LeaseID == firstRun.req.LeaseID || replacement.req.Fence == firstRun.req.Fence {
		t.Fatal("test did not replace physical attempt")
	}
	replay, err := s.RequestChildFromExecution(context.Background(), "alice", session.ID, replacement.req.TurnID, replacement.req.LeaseID, replacement.req.Fence, "logical-tool-call", "review", "name: review\n")
	if err != nil || replay != first {
		t.Fatalf("attempt replacement duplicated logical proposal: %+v %v", replay, err)
	}
	current, _ := s.Session("alice", session.ID)
	if len(current.Children) != 1 {
		t.Fatal("retry reset child budget")
	}
	_, err = s.RequestChildFromExecution(context.Background(), "alice", session.ID, firstRun.req.TurnID, firstRun.req.LeaseID, firstRun.req.Fence, "logical-tool-call", "review", "name: review\n")
	requireCode(t, err, "stale_fence")
	replacement.result <- ExecutionResult{}
	waitStatus(t, s, "alice", session.ID, "idle")
}
