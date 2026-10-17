package interactiveops

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/platform/lock"
)

func TestConfigRecoveryObservesCommittedOrRolledBackGeneration(t *testing.T) {
	for _, committed := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed", false: "rolled-back"}[committed], func(t *testing.T) {
			svc, scope := fixture(t, "")
			ctx := context.Background()
			name := "Recovered fixture"
			a, err := svc.Propose(ctx, scope, Request{Kind: "tweak_gaggle", DisplayName: &name, IdempotencyKey: "change"})
			if err != nil {
				t.Fatal(err)
			}
			dir := svc.scopeDir(scope)
			if committed {
				if err = svc.applyConfig(scope, dir, a.Request); err != nil {
					t.Fatal(err)
				}
			}
			a.Status = "running"
			a.OperationStatus = "dispatched"
			if err = saveAction(dir, &a); err != nil {
				t.Fatal(err)
			}
			recovered, err := svc.Get(ctx, scope, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "failed"
			if committed {
				want = "succeeded"
			}
			if recovered.Status != want {
				t.Fatalf("recovered %+v", recovered)
			}
		})
	}
}
func TestCrossInstanceAdmissionBudgetsAndUnknownCustody(t *testing.T) {
	svc, scope := fixture(t, "")
	svc.opts.MaxActive = 2
	svc.opts.MaxActivePerOwner = 1
	peer, err := New(svc.opts)
	if err != nil {
		t.Fatal(err)
	}
	scopes := []Scope{scope, {ActorID: "alice", SessionID: "second"}, {ActorID: "bob", SessionID: "first"}, {ActorID: "carol", SessionID: "first"}}
	for _, scope := range scopes {
		if _, err = svc.Snapshot(context.Background(), scope); err != nil {
			t.Fatal(err)
		}
	}
	first, err := svc.reserveWorker(scopes[0], svc.scopeDir(scopes[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if _, err = peer.reserveWorker(scopes[1], svc.scopeDir(scopes[1])); !errors.Is(err, ErrCapacity) {
		t.Fatalf("owner limit not enforced: %v", err)
	}
	second, err := peer.reserveWorker(scopes[2], svc.scopeDir(scopes[2]))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if _, err = svc.reserveWorker(scopes[3], svc.scopeDir(scopes[3])); !errors.Is(err, ErrCapacity) {
		t.Fatalf("global limit not enforced: %v", err)
	}
	_ = first.Release()
	third, err := peer.reserveWorker(scopes[1], svc.scopeDir(scopes[1]))
	if err != nil {
		t.Fatal(err)
	}
	_ = third.Release()
	a := Action{ID: "op_" + strings.Repeat("a", 32), Kind: "submit_run", Status: "uncertain", OperationStatus: "outcome_unknown"}
	if err = saveAction(svc.scopeDir(scopes[1]), &a); err != nil {
		t.Fatal(err)
	}
	if _, err = peer.reserveWorker(scopes[3], svc.scopeDir(scopes[3])); !errors.Is(err, ErrCapacity) {
		t.Fatalf("unknown custody released budget: %v", err)
	}
}
func TestStopRefusesUnknownCustodyAndFencesAllInstances(t *testing.T) {
	svc, scope := fixture(t, "")
	if _, err := svc.Snapshot(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	dir := svc.scopeDir(scope)
	a := Action{ID: "op_" + strings.Repeat("b", 32), Kind: "submit_run", Status: "uncertain", OperationStatus: "outcome_unknown"}
	if err := saveAction(dir, &a); err != nil {
		t.Fatal(err)
	}
	if err := svc.StopSession(context.Background(), scope); err == nil {
		t.Fatal("unconfirmed custody reported stopped")
	}
	peer, err := New(svc.opts)
	if err != nil {
		t.Fatal(err)
	}
	name := "blocked"
	if _, err = peer.Propose(context.Background(), scope, Request{Kind: "tweak_gaggle", IdempotencyKey: "new", DisplayName: &name}); err == nil {
		t.Fatal("peer bypassed stop fence")
	}
}
func TestHTTPUsesServerBoundIdentityAndStrictRequests(t *testing.T) {
	svc, scope := fixture(t, "")
	handler := svc.Handler(func(_ context.Context, id string) (Scope, error) {
		if id != scope.SessionID {
			return Scope{}, errors.New("session denied")
		}
		return scope, nil
	})
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "state?sessionId=other", "", 403},
		{"POST", "proposals?sessionId=session-a", `{"kind":"tweak_gaggle","idempotencyKey":"one","displayName":"x","actorId":"bob"}`, 409},
		{"GET", "state?sessionId=session-a", "", 200},
	} {
		r := httptest.NewRequest(tc.method, "/api/v1/interactive/operations/"+tc.path, strings.NewReader(tc.body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
}
func TestChatHonorsShortExplicitWorkflowAndRejectsUnknown(t *testing.T) {
	svc, scope := fixture(t, os.Args[0])
	ctx := context.Background()
	state, err := svc.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	draft := strings.Replace(state.WorkflowDraft, "name: invoice-check-custom", "name: smoke", 1)
	a, err := svc.Propose(ctx, scope, Request{Kind: "add_workflow", WorkflowYAML: draft, IdempotencyKey: "short"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Apply(ctx, scope, a.ID); err != nil {
		t.Fatal(err)
	}
	a, err = svc.Chat(ctx, scope, "run workflow smoke", "select")
	if err != nil || a.Request.Workflow != "smoke" {
		t.Fatalf("wrong target: %+v %v", a, err)
	}
	if _, err = svc.Chat(ctx, scope, "run workflow typo", "bad"); err == nil {
		t.Fatal("unknown name silently defaulted")
	}
}
func TestBusyAdmissionDoesNotDispatch(t *testing.T) {
	svc, scope := fixture(t, os.Args[0])
	ctx := context.Background()
	if _, err := svc.Snapshot(ctx, scope); err != nil {
		t.Fatal(err)
	}
	held, err := lock.TryAcquire(filepath.Join(svc.scopeDir(scope), "worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	a, err := svc.Propose(ctx, scope, Request{Kind: "submit_run", Workflow: "invoice-check", IdempotencyKey: "busy"})
	if err != nil {
		t.Fatal(err)
	}
	a, err = svc.Apply(ctx, scope, a.ID)
	if err != nil || a.Status != "proposed" || a.Waiting != "capacity" || a.OperationStatus != "prepared" {
		t.Fatalf("capacity dispatch state: %+v %v", a, err)
	}
}
