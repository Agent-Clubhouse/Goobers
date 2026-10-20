package interactiveauth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixture struct {
	b        *Broker
	p        Principal
	other    Principal
	ado      Connection
	insights Connection
	a        Authority
	now      time.Time
	path     string
}

func newFixture(t *testing.T, budget int) *fixture {
	t.Helper()
	f := &fixture{now: time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC), path: filepath.Join(t.TempDir(), "authority.json")}
	var err error
	f.b, err = Open(Config{Path: f.path, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.b.Close() })
	f.p, _ = LocalPrincipal("owner-one")
	f.other, _ = LocalPrincipal("owner-two")
	f.ado, err = f.b.ConnectDemo(context.Background(), f.p, ADO)
	if err != nil {
		t.Fatal(err)
	}
	f.insights, err = f.b.ConnectDemo(context.Background(), f.p, AppInsights)
	if err != nil {
		t.Fatal(err)
	}
	f.a, err = f.b.IssueAuthority(context.Background(), f.p, AuthorityRequest{SessionID: "session-one", ExecutionID: "turn-one", ExecutionFence: 3, Bindings: []Binding{{f.ado.Ref, f.ado.Resources}, {f.insights.Ref, f.insights.Resources}}, AllowedActions: []Action{ReadWorkItem, QueryTelemetry, AddComment}, NotAfter: f.now.Add(time.Hour), Budget: budget, MaxChildren: 2, MaxDepth: 2})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) read(id string) OperationRequest {
	return OperationRequest{Authority: f.a.Ref, SessionID: f.a.SessionID, ExecutionID: f.a.ExecutionID, ExecutionFence: f.a.ExecutionFence, OperationID: id, ActionID: "action-" + id, IdempotencyKey: "key-" + id, Action: ReadWorkItem, Resource: DemoADOResource(), Payload: Payload{WorkItemID: 427}}
}
func (f *fixture) write(t *testing.T, id string, loss bool) OperationRequest {
	t.Helper()
	r := f.read(id)
	r.Action = AddComment
	r.Payload.Comment = "Investigated locally; health regression test passes."
	r.SimulateLostResponse = loss
	d, err := f.b.Authorize(context.Background(), f.p, r, f.now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	r.DecisionID = d.ID
	return r
}
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if ErrorCode(err) != code {
		t.Fatalf("code = %q, want %q (err %v)", ErrorCode(err), code, err)
	}
}
func (f *fixture) childRequest() AuthorityRequest {
	return AuthorityRequest{SessionID: f.a.SessionID, ExecutionID: "child-one", ExecutionFence: 3, Bindings: []Binding{{f.ado.Ref, f.ado.Resources}}, AllowedActions: []Action{ReadWorkItem}, NotAfter: f.a.NotAfter, Budget: 500, MaxChildren: 0, MaxDepth: 2}
}

func TestPrincipalAndResourceIsolation(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	_, err := f.b.ListConnections(Principal{})
	requireCode(t, err, "unauthenticated")
	var forged Principal
	//nolint:staticcheck // SA9005: deliberately verify JSON cannot forge private principal fields.
	_ = json.Unmarshal([]byte(`{"identity":{"subject":"owner-one"},"verified":true}`), &forged)
	_, err = f.b.ListConnections(forged)
	requireCode(t, err, "unauthenticated")
	_, err = f.b.Execute(ctx, f.other, f.read("foreign"))
	requireCode(t, err, "not_found")
	err = f.b.RevokeConnection(ctx, f.other, f.ado.Ref)
	requireCode(t, err, "not_found")
	connections, err := f.b.ListConnections(f.other)
	if err != nil || len(connections) != 0 {
		t.Fatalf("foreign connections: %v, %v", connections, err)
	}
	for _, mutate := range []func(*OperationRequest){func(r *OperationRequest) { r.Resource.Organization = "other" }, func(r *OperationRequest) { r.Resource.Project = "other" }, func(r *OperationRequest) { r.Resource.Repository = "*" }, func(r *OperationRequest) { r.Resource.Repository = "sample-app/sub" }, func(r *OperationRequest) { r.Resource.Provider = AppInsights }} {
		r := f.read(newID("scope_"))
		mutate(&r)
		_, err = f.b.Execute(ctx, f.p, r)
		requireCode(t, err, "forbidden_resource")
	}
	r := f.read("wrong-session")
	r.SessionID = "other-session"
	_, err = f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "execution_mismatch")
	r = f.read("wrong-fence")
	r.ExecutionFence++
	_, err = f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "stale_fence")
	r = f.read("wrong-revision")
	r.Authority.Version++
	_, err = f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "stale_authority")
	ops, _ := f.b.ListOperations(f.p, "")
	if len(ops) != 0 {
		t.Fatalf("denied calls produced dispatches: %v", ops)
	}
}

func TestImmutableSnapshotsAndBindings(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	f.a.Bindings[0].Resources[0].Repository = "changed"
	f.a.AllowedActions[0] = "unbounded"
	connections, _ := f.b.ListConnections(f.p)
	for i := range connections {
		connections[i].Resources[0].Repository = "changed"
	}
	op, err := f.b.Execute(ctx, f.p, f.read("read"))
	if err != nil || op.State != "confirmed" {
		t.Fatalf("caller mutated authority: %+v %v", op, err)
	}
	op.Result["mode"] = "real"
	stored, _ := f.b.ListOperations(f.p, "")
	if stored[0].Result["mode"] != ControlledLocal {
		t.Fatal("operation result aliases public map")
	}
	bad := f.childRequest()
	bad.Bindings[0].Connection.Revision++
	_, err = f.b.DeriveChild(ctx, f.p, f.a.Ref, bad)
	requireCode(t, err, "stale_binding")
}

func TestRefreshReauthenticationAndDeadlineRemainSeparate(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	before := f.a.NotAfter
	if err := f.b.SetDemoAuthState(ctx, f.p, f.ado.Ref, "expired"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.Execute(ctx, f.p, f.read("renew")); err != nil {
		t.Fatal(err)
	}
	connections, _ := f.b.ListConnections(f.p)
	for _, c := range connections {
		if c.Provider == ADO && (c.Ref != f.ado.Ref || c.RefreshCount != 1 || c.State != "ready") {
			t.Fatalf("bad refresh: %+v", c)
		}
	}
	if err := f.b.SetDemoAuthState(ctx, f.p, f.ado.Ref, "interaction_required"); err != nil {
		t.Fatal(err)
	}
	_, err := f.b.Execute(ctx, f.p, f.read("pause"))
	requireCode(t, err, "interaction_required")
	_, err = f.b.Reauthenticate(ctx, f.other, f.ado.Ref)
	requireCode(t, err, "not_found")
	_, err = f.b.Reauthenticate(ctx, f.p, f.ado.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.b.Execute(ctx, f.p, f.read("resume")); err != nil {
		t.Fatal(err)
	}
	authorities, _ := f.b.ListAuthorities(f.p)
	if !authorities[0].Authority.NotAfter.Equal(before) || authorities[0].Budget.Used != 2 {
		t.Fatal("reauth reset lifetime or used budget")
	}
	f.now = before
	_, err = f.b.Reauthenticate(ctx, f.p, f.ado.Ref)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.b.Execute(ctx, f.p, f.read("expired-authority"))
	requireCode(t, err, "authority_expired")
}

func TestConsentAndRevocationCannotSilentlyFallBack(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	if err := f.b.SetDemoAuthState(ctx, f.p, f.ado.Ref, "consent_required"); err != nil {
		t.Fatal(err)
	}
	_, err := f.b.Execute(ctx, f.p, f.read("consent"))
	requireCode(t, err, "consent_required")
	if err = f.b.RevokeConnection(ctx, f.p, f.ado.Ref); err != nil {
		t.Fatal(err)
	}
	_, err = f.b.Reauthenticate(ctx, f.p, f.ado.Ref)
	requireCode(t, err, "connection_revoked")
	_, err = f.b.Execute(ctx, f.p, f.read("revoked"))
	requireCode(t, err, "connection_revoked")
	c, err := f.b.ConnectDemo(ctx, f.p, ADO)
	if err != nil || c.Ref != f.ado.Ref || c.State != "revoked" {
		t.Fatalf("connect revived revoked identity: %+v %v", c, err)
	}
	if err = f.b.Close(); err != nil {
		t.Fatal(err)
	}
	f.b, err = Open(Config{Path: f.path, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	c, err = f.b.ConnectDemo(ctx, f.p, ADO)
	if err != nil || c.State != "revoked" {
		t.Fatal("restart lost canonical revocation")
	}
}

func TestChildrenAttenuateAndShareBudget(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	for _, mutate := range []func(*AuthorityRequest){func(r *AuthorityRequest) { r.NotAfter = r.NotAfter.Add(time.Second) }, func(r *AuthorityRequest) { r.SessionID = "other" }, func(r *AuthorityRequest) { r.MaxDepth = 4 }, func(r *AuthorityRequest) { r.MaxChildren = 4 }} {
		r := f.childRequest()
		mutate(&r)
		_, err := f.b.DeriveChild(ctx, f.p, f.a.Ref, r)
		requireCode(t, err, "attenuation_denied")
	}
	child, err := f.b.DeriveChild(ctx, f.p, f.a.Ref, f.childRequest())
	if err != nil {
		t.Fatal(err)
	}
	if child.SharedBudgetID != f.a.SharedBudgetID {
		t.Fatal("child acquired fresh budget")
	}
	_, err = f.b.Execute(ctx, f.p, f.read("parent-op"))
	if err != nil {
		t.Fatal(err)
	}
	r := f.read("child-op")
	r.Authority = child.Ref
	r.ExecutionID = child.ExecutionID
	_, err = f.b.Execute(ctx, f.p, r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.b.Execute(ctx, f.p, f.read("no-more-budget"))
	requireCode(t, err, "budget_exhausted")
	r = f.read("no-child-budget")
	r.Authority = child.Ref
	r.ExecutionID = child.ExecutionID
	_, err = f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "budget_exhausted")
	if err = f.b.StopAuthority(ctx, f.p, f.a.Ref); err != nil {
		t.Fatal(err)
	}
	r.OperationID = "stopped-child"
	r.IdempotencyKey = "stopped-key"
	_, err = f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "authority_stopped")
	_, err = f.b.DeriveChild(ctx, f.p, f.a.Ref, f.childRequest())
	requireCode(t, err, "authority_stopped")
}

func TestChildCannotAcquireConnectionOrAction(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	req := f.childRequest()
	parent, err := f.b.IssueAuthority(ctx, f.p, AuthorityRequest{SessionID: "limited", ExecutionID: "limited-turn", ExecutionFence: 1, Bindings: req.Bindings, AllowedActions: []Action{ReadWorkItem}, NotAfter: f.a.NotAfter, Budget: 5, MaxChildren: 1, MaxDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	req.SessionID = parent.SessionID
	req.MaxDepth = 1
	req.AllowedActions = append(req.AllowedActions, AddComment)
	_, err = f.b.DeriveChild(ctx, f.p, parent.Ref, req)
	requireCode(t, err, "attenuation_denied")
	req.AllowedActions = []Action{ReadWorkItem}
	req.Bindings = append(req.Bindings, Binding{f.insights.Ref, f.insights.Resources})
	_, err = f.b.DeriveChild(ctx, f.p, parent.Ref, req)
	requireCode(t, err, "attenuation_denied")
	req.Bindings = req.Bindings[:1]
	child, err := f.b.DeriveChild(ctx, f.p, parent.Ref, req)
	if err != nil {
		t.Fatal(err)
	}
	req.ExecutionID = "too-many"
	_, err = f.b.DeriveChild(ctx, f.p, parent.Ref, req)
	requireCode(t, err, "child_limit")
	req.ExecutionID = "grandchild"
	_, err = f.b.DeriveChild(ctx, f.p, child.Ref, req)
	requireCode(t, err, "child_limit")
}

func TestExactApprovalAndIdempotentReplay(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	r := f.read("no-decision")
	r.Action = AddComment
	r.Payload.Comment = "Reviewed change."
	_, err := f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "decision_required")
	r = f.write(t, "comment", false)
	changed := r
	changed.Payload.Comment = "Different effect"
	_, err = f.b.Execute(ctx, f.p, changed)
	requireCode(t, err, "decision_required")
	op, err := f.b.Execute(ctx, f.p, r)
	if err != nil || op.State != "confirmed" {
		t.Fatalf("write: %+v %v", op, err)
	}
	replayed, err := f.b.Execute(ctx, f.p, r)
	if err != nil || replayed.ID != op.ID {
		t.Fatalf("replay: %+v %v", replayed, err)
	}
	_, err = f.b.Execute(ctx, f.p, changed)
	requireCode(t, err, "idempotency_conflict")
	duplicate := r
	duplicate.OperationID = "second-id"
	_, err = f.b.Execute(ctx, f.p, duplicate)
	requireCode(t, err, "idempotency_conflict")
	states, _ := f.b.ListAuthorities(f.p)
	if states[0].Budget.Used != 1 || len(f.b.data.DemoComments) != 1 {
		t.Fatal("replay dispatched a second effect")
	}
	if err = f.b.RevokeConnection(ctx, f.p, f.ado.Ref); err != nil {
		t.Fatal(err)
	}
	replayed, err = f.b.Execute(ctx, f.p, r)
	if err != nil || replayed.State != "confirmed" {
		t.Fatal("historical receipts should remain available after revocation")
	}
}

func TestReauthenticationDoesNotExtendExactDecision(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	r := f.write(t, "expires", false)
	f.now = f.now.Add(6 * time.Minute)
	_, err := f.b.Reauthenticate(ctx, f.p, f.ado.Ref)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "decision_expired")
}

func TestUnknownEffectNeverBlindlyReplaysAndCanReconcileAfterRevoke(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	r := f.write(t, "lost", true)
	op, err := f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "outcome_unknown")
	if op.State != "outcome_unknown" || len(f.b.data.DemoComments) != 1 {
		t.Fatal("lost-response fixture did not actually commit its controlled effect")
	}
	_, err = f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "outcome_unknown")
	if len(f.b.data.DemoComments) != 1 {
		t.Fatal("unknown write replayed")
	}
	if err = f.b.Close(); err != nil {
		t.Fatal(err)
	}
	f.b, err = Open(Config{Path: f.path, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "outcome_unknown")
	if err = f.b.RevokeConnection(ctx, f.p, f.ado.Ref); err != nil {
		t.Fatal(err)
	}
	op, err = f.b.Reconcile(ctx, f.p, r.OperationID)
	if err != nil || op.State != "confirmed" {
		t.Fatalf("reconcile: %+v %v", op, err)
	}
	if len(f.b.data.DemoComments) != 1 {
		t.Fatal("reconciliation resubmitted effect")
	}
	_, err = f.b.Reconcile(ctx, f.other, r.OperationID)
	requireCode(t, err, "not_found")
}

func TestCrashAfterDispatchMarkerStaysUnknown(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	r := f.read("crashed")
	f.b.afterAdmission = func() { _ = f.b.Close() }
	op, err := f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "outcome_unknown")
	if op.State != "dispatched" {
		t.Fatal(op.State)
	}
	f.b, err = Open(Config{Path: f.path, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	op, err = f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "outcome_unknown")
	if op.State != "outcome_unknown" {
		t.Fatal(op.State)
	}
	_, err = f.b.Reconcile(ctx, f.p, r.OperationID)
	requireCode(t, err, "outcome_unknown")
	connections, _ := f.b.ListConnections(f.p)
	for _, c := range connections {
		if c.State != "interaction_required" {
			t.Fatal("restart silently assumed renewal")
		}
	}
}

func TestRevokeSerializesAdmissionButDoesNotClaimToUndoInFlight(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	r := f.write(t, "inflight", false)
	admitted := make(chan struct{})
	proceed := make(chan struct{})
	f.b.afterAdmission = func() { close(admitted); <-proceed }
	var op Operation
	var callErr error
	done := make(chan struct{})
	go func() { op, callErr = f.b.Execute(ctx, f.p, r); close(done) }()
	<-admitted
	if err := f.b.RevokeConnection(ctx, f.p, f.ado.Ref); err != nil {
		t.Fatal(err)
	}
	_, err := f.b.Execute(ctx, f.p, f.read("after-revoke"))
	requireCode(t, err, "connection_revoked")
	close(proceed)
	<-done
	if callErr != nil || op.State != "confirmed" {
		t.Fatalf("already admitted effect incorrectly erased: %+v %v", op, callErr)
	}
	if op.ConnectionEpoch != 1 {
		t.Fatal("operation did not capture pre-revocation fence")
	}
}

func TestConcurrentReplayConsumesOneBudget(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	r := f.read("concurrent")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.b.Execute(ctx, f.p, r); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	states, _ := f.b.ListAuthorities(f.p)
	if states[0].Budget.Used != 1 {
		t.Fatal("concurrent stable ID dispatched repeatedly")
	}
	ops, _ := f.b.ListOperations(f.p, "")
	if len(ops) != 1 || ops[0].State != "confirmed" {
		t.Fatal(ops)
	}
}

func TestCancelledAdmissionDoesNotCreateEffect(t *testing.T) {
	f := newFixture(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	r := f.write(t, "cancel", false)
	f.b.afterAdmission = cancel
	op, err := f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "cancelled")
	if op.State != "failed" || len(f.b.data.DemoComments) != 0 {
		t.Fatal("cancelled controlled request sent")
	}
}

func TestStoreLockAndCorruptionFailClosed(t *testing.T) {
	f := newFixture(t, 10)
	_, err := Open(Config{Path: f.path})
	requireCode(t, err, "unavailable")
	if err = f.b.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.path, []byte(`{"schema":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(Config{Path: f.path})
	requireCode(t, err, "unavailable")
}

func TestStorageFailureFencesFurtherEffects(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	f.b.path = filepath.Join(f.path, "impossible-child")
	_, err := f.b.Execute(ctx, f.p, f.read("failed-write"))
	requireCode(t, err, "unavailable")
	_, err = f.b.Execute(ctx, f.p, f.read("fenced"))
	requireCode(t, err, "unavailable")
	if len(f.b.data.DemoComments) != 0 {
		t.Fatal("provider invoked despite failed dispatch persistence")
	}
}

func TestPayloadCredentialDefenseAndExplicitProviderLabel(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	r := f.read("secret")
	r.Action = AddComment
	r.Payload.Comment = "Bearer " + strings.Repeat("a", 32)
	_, err := f.b.Authorize(ctx, f.p, r, f.now.Add(time.Minute))
	requireCode(t, err, "secret_rejected")
	r = f.read("telemetry")
	r.Action = QueryTelemetry
	r.Resource = DemoInsightsResource()
	r.Payload = Payload{Query: "requests | summarize count() by resultCode"}
	op, err := f.b.Execute(ctx, f.p, r)
	if err != nil || op.Mode != ControlledLocal || op.Result["mode"] != ControlledLocal {
		t.Fatalf("missing fixture label: %+v %v", op, err)
	}
	r = f.read("arbitrary-query")
	r.Action = QueryTelemetry
	r.Resource = DemoInsightsResource()
	r.Payload = Payload{Query: "union app('other-tenant').requests"}
	_, err = f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "unsupported_query")
	data, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), strings.Repeat("a", 32)) || strings.Contains(string(data), "access_token") || strings.Contains(string(data), "refresh_token") {
		t.Fatal("credential material reached persistent ledger")
	}
	info, _ := os.Stat(f.path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("authority file permissions %o", info.Mode().Perm())
	}
}

func TestRecoveryRevisionPreservesBudgetDeadlineAndFencesPriorChildren(t *testing.T) {
	f := newFixture(t, 4)
	ctx := context.Background()
	original := f.read("before-recovery")
	if _, err := f.b.Execute(ctx, f.p, original); err != nil {
		t.Fatal(err)
	}
	child, err := f.b.DeriveChild(ctx, f.p, f.a.Ref, f.childRequest())
	if err != nil {
		t.Fatal(err)
	}
	next, err := f.b.RebindExecution(ctx, f.p, f.a.Ref, 4)
	if err != nil {
		t.Fatal(err)
	}
	if next.Ref.ID != f.a.Ref.ID || next.Ref.Version != 2 || next.SharedBudgetID != f.a.SharedBudgetID || !next.NotAfter.Equal(f.a.NotAfter) || next.ExecutionFence != 4 {
		t.Fatalf("recovery widened/reset grant: %+v", next)
	}
	again, err := f.b.RebindExecution(ctx, f.p, f.a.Ref, 4)
	if err != nil || again.Ref != next.Ref {
		t.Fatalf("rebind replay: %+v %v", again, err)
	}
	_, err = f.b.RebindExecution(ctx, f.p, f.a.Ref, 5)
	requireCode(t, err, "idempotency_conflict")
	_, err = f.b.Execute(ctx, f.p, f.read("old-attempt"))
	requireCode(t, err, "authority_stopped")
	r := f.read("old-child")
	r.Authority = child.Ref
	r.ExecutionID = child.ExecutionID
	_, err = f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "authority_stopped")
	r = f.read("recovered")
	r.Authority = next.Ref
	r.ExecutionFence = 4
	if _, err = f.b.Execute(ctx, f.p, r); err != nil {
		t.Fatal(err)
	}
	reused := r
	reused.OperationID = "duplicate-after-recovery"
	reused.IdempotencyKey = original.IdempotencyKey
	_, err = f.b.Execute(ctx, f.p, reused)
	requireCode(t, err, "idempotency_conflict")
	states, _ := f.b.ListAuthorities(f.p)
	if len(states) != 3 {
		t.Fatal("immutable revision history was replaced")
	}
	for _, state := range states {
		if state.Budget.Used != 2 {
			t.Fatal("recovery created or reset shared budget")
		}
	}
	if err = f.b.StopAuthority(ctx, f.p, next.Ref); err != nil {
		t.Fatal(err)
	}
	_, err = f.b.RebindExecution(ctx, f.p, next.Ref, 5)
	requireCode(t, err, "authority_stopped")
}

func TestRecoveryCannotReviveRevokedOrExpiredAuthority(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		f := newFixture(t, 4)
		ctx := context.Background()
		if err := f.b.RevokeConnection(ctx, f.p, f.ado.Ref); err != nil {
			t.Fatal(err)
		}
		_, err := f.b.RebindExecution(ctx, f.p, f.a.Ref, 4)
		requireCode(t, err, "connection_revoked")
	})
	t.Run("expired", func(t *testing.T) {
		f := newFixture(t, 4)
		f.now = f.a.NotAfter
		_, err := f.b.RebindExecution(context.Background(), f.p, f.a.Ref, 4)
		requireCode(t, err, "authority_expired")
	})
}

func TestFinalAdmissionChecksVerifiedIdentityAndCurrentExecution(t *testing.T) {
	f := newFixture(t, 4)
	ctx := context.Background()
	allow := false
	calls := 0
	f.b.checkExecution = func(_ context.Context, identity Identity, req OperationRequest) error {
		calls++
		if identity != f.p.Identity() {
			t.Fatal("callback did not receive verified broker actor")
		}
		if req.TurnID != "current-turn" || req.LeaseID != "current-lease" {
			return fault("stale_fence", "private lease error")
		}
		if !allow {
			return fault("unavailable", "provider Bearer "+strings.Repeat("a", 32))
		}
		return nil
	}
	r := f.read("lease-checked")
	_, err := f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "execution_unavailable")
	if calls != 0 {
		t.Fatal("missing runtime binding reached lease callback")
	}
	r.TurnID = "current-turn"
	r.LeaseID = "current-lease"
	_, err = f.b.Execute(ctx, f.p, r)
	requireCode(t, err, "execution_unavailable")
	if strings.Contains(err.Error(), "Bearer") {
		t.Fatal("callback's unsafe diagnostic escaped broker")
	}
	states, _ := f.b.ListAuthorities(f.p)
	if states[0].Budget.Used != 0 {
		t.Fatal("failed lease check admitted or charged operation")
	}
	allow = true
	if _, err = f.b.Execute(ctx, f.p, r); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected final lease check, got %d calls", calls)
	}
}
