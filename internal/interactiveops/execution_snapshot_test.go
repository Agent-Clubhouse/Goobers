package interactiveops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRunSnapshotCallbackFailureDoesNotDispatchOrRetry(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "unused-native-binary")
	if err := os.WriteFile(binary, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	s, scope := fixture(t, binary)
	ctx := context.Background()
	calls := 0
	s.opts.RunRepositoryForScope = func(context.Context, Scope, string) (string, error) {
		calls++
		return "", errors.New("parent workspace is not idle")
	}
	a, err := s.Propose(ctx, scope, Request{Kind: "submit_run", Workflow: "invoice-check", IdempotencyKey: "snapshot-busy"})
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.Apply(ctx, scope, a.ID)
	if err != nil || a.Status != "failed" || a.OperationStatus != "failed" || a.ExecutionRoot != "" {
		t.Fatalf("snapshot refusal dispatched: %+v %v", a, err)
	}
	replay, err := s.Apply(ctx, scope, a.ID)
	if err != nil || replay.Status != "failed" || calls != 1 {
		t.Fatalf("snapshot refusal replayed capture: %+v calls=%d %v", replay, calls, err)
	}
	if err = s.StopSession(ctx, scope); err != nil {
		t.Fatal(err)
	}
}
func TestRunSnapshotPolicyChangePreventsDispatch(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "unused-native-binary")
	if err := os.WriteFile(binary, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	s, scope, p := scopedNativeService(t, binary)
	ctx := context.Background()
	state, err := s.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	captured := t.TempDir()
	s.opts.RunRepositoryForScope = func(context.Context, Scope, string) (string, error) {
		if err := copyTree(state.Repository, captured); err != nil {
			return "", err
		}
		p.Revision = "changed-during-capture"
		return captured, nil
	}
	a, err := s.Propose(ctx, scope, Request{Kind: "submit_run", Workflow: "selected-check", IdempotencyKey: "changed-during-capture"})
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.Apply(ctx, scope, a.ID)
	if err != nil || a.Status != "failed" || a.OperationStatus != "failed" || a.RunID != "" {
		t.Fatalf("revoked capture dispatched: %+v %v", a, err)
	}
	receipt, err := s.Get(ctx, scope, a.ID)
	if err != nil || receipt.OperationStatus != "failed" {
		t.Fatalf("failed receipt unavailable: %+v %v", receipt, err)
	}
}
func TestExecutionRootCannotReadAnotherScope(t *testing.T) {
	s, scope := fixture(t, "")
	ctx := context.Background()
	state, err := s.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	a := Action{ID: "op_0123456789abcdef0123456789abcdef", ExecutionRoot: filepath.Join(t.TempDir(), "instance")}
	if _, err = actionExecutionRoot(s.scopeDir(scope), &a); err == nil {
		t.Fatal("foreign native receipt root accepted")
	}
	if err = saveAction(s.scopeDir(scope), &a); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SnapshotRecorded(ctx, scope); err == nil {
		t.Fatal("historical read followed foreign execution root")
	}
	if _, err = os.Stat(state.InstanceRoot); err != nil {
		t.Fatal(err)
	}
}
