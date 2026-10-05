package sessionops

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

type recorder struct {
	mu     sync.Mutex
	events []journal.Event
	data   [][]byte
	fail   bool
}

func (r *recorder) Append(e journal.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("audit unavailable")
	}
	copy := e
	copy.Runner = map[string]any{}
	for k, v := range e.Runner {
		copy.Runner[k] = v
	}
	r.events = append(r.events, copy)
	return nil
}
func (r *recorder) RecordArtifactWithIntegrity(_ string, b []byte, grade apiv1.Integrity) (journal.Ref, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if grade != apiv1.IntegrityUnapproved {
		return journal.Ref{}, errors.New("incorrect source trust")
	}
	r.data = append(r.data, append([]byte(nil), b...))
	return journal.Ref{Digest: journal.Digest(b)}, nil
}

type reader struct {
	get func(context.Context, string, workbench.BacklogItemRequest) (workbench.BacklogItem, error)
}

func (r reader) Get(ctx context.Context, b string, q workbench.BacklogItemRequest) (workbench.BacklogItem, error) {
	return r.get(ctx, b, q)
}
func (r reader) Page(context.Context, string, workbench.BacklogPageRequest) (workbench.BacklogPage, error) {
	return workbench.BacklogPage{}, nil
}

type fixture struct {
	bridge   *Bridge
	inv      Invocation
	registry *journal.RegistryScrubber
	rec      *recorder
	cancel   context.CancelFunc
}

func fixtureFor(t *testing.T) *fixture {
	t.Helper()
	reg := journal.NewRegistryScrubber()
	actor := sessioning.Actor{Issuer: "https://identity.example", Subject: "alice"}
	gaggle := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: apiv1.GaggleSpec{InteractiveAccess: &apiv1.InteractiveAccessPolicy{Actions: []apiv1.InteractiveAction{"session.message", "backlog.read"}, Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: actor.Issuer, Subject: actor.Subject}}}}}}
	permissions, err := interactiveaccess.New([]apiv1.Gaggle{gaggle}, nil, interactiveaccess.Dependencies{Registrar: reg})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	lease, err := permissions.BeginSessionExecution(ctx, httpapi.Principal{Issuer: actor.Issuer, Subject: actor.Subject, Roles: []httpapi.Role{httpapi.RoleOperate}}, "web")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Close)
	run := strings.Repeat("a", 32)
	digest := "sha256:" + strings.Repeat("b", 64)
	id := journal.RunIdentity{InstanceID: "instance", RunID: run, Gaggle: "web", Workflow: "interactive-session", ConfigGeneration: digest, WorkflowDigest: digest, GooberDigest: digest, Trigger: journal.Trigger{Kind: journal.TriggerSignal, Ref: "session:session-one:turn-one"}, Session: &journal.SessionLineage{Gaggle: "web", SessionID: "session-one", TurnID: "turn-one", MessageID: "message-one", AcceptanceID: "trigger-" + run, EnvelopeDigest: digest, InputDigest: digest}}
	rec := &recorder{}
	b := &Bridge{Endpoint: "https://daemon.invalid", Scrubber: reg, Secrets: reg, Now: time.Now}
	inv := Invocation{Identity: id, Actor: actor, StageSequence: 3, Attempt: 1, Lease: lease, Recorder: rec, Reader: reader{get: func(_ context.Context, b string, q workbench.BacklogItemRequest) (workbench.BacklogItem, error) {
		return item(b, q.ID), nil
	}}}
	return &fixture{bridge: b, inv: inv, rec: rec, registry: reg, cancel: cancel}
}
func item(binding, id string) workbench.BacklogItem {
	return workbench.BacklogItem{Ref: workbench.NodeRef{GaggleID: "web", SourceBindingID: binding, Kind: "work-item", SourceID: "native-node"}, Locator: workbench.SourceLocator{ID: id}, Title: "Feature", Type: "issue", State: "open", RevisionSemantics: "preflight", RelationshipCoverage: workbench.RelationshipCoverage{Parents: "not-loaded", Blockers: "not-loaded", Milestones: "not-loaded"}}
}
func open(t *testing.T, f *fixture) *mcpio.SessionOperationAccess {
	t.Helper()
	a, close, err := f.bridge.Open(f.inv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(close)
	return a
}
func readRequest() sessioning.BacklogReadRequest {
	return sessioning.BacklogReadRequest{SourceBindingID: "backlog", BacklogItemRequest: workbench.BacklogItemRequest{ID: "42"}}
}
func TestSessionOperationReadAuditsHumanAndUnapprovedData(t *testing.T) {
	f := fixtureFor(t)
	f.registry.Register([]byte("secret-canary"))
	f.inv.Reader = reader{get: func(_ context.Context, b string, q workbench.BacklogItemRequest) (workbench.BacklogItem, error) {
		v := item(b, q.ID)
		v.Description = "source secret-canary"
		return v, nil
	}}
	access := open(t, f)
	result, err := f.bridge.GetBacklogItem(t.Context(), access.BearerToken, f.inv.Identity.RunID, readRequest())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Description, "secret-canary") || len(f.rec.events) != 2 || len(f.rec.data) != 1 {
		t.Fatalf("result/audit = %+v / %+v", result, f.rec.events)
	}
	for _, e := range f.rec.events {
		if e.Runner["humanIssuer"] != f.inv.Actor.Issuer || e.Runner["humanSubject"] != f.inv.Actor.Subject || e.Runner["turnId"] != "turn-one" {
			t.Fatal("lost attribution")
		}
	}
	if strings.Contains(string(f.rec.data[0]), access.BearerToken) {
		t.Fatal("grant persisted")
	}
	if _, err = f.bridge.GetBacklogItem(t.Context(), access.BearerToken, "other", readRequest()); err == nil {
		t.Fatal("cross-run access")
	}
	f.cancel()
	if _, err = f.bridge.AuthenticateSessionOperation(access.BearerToken); err == nil {
		t.Fatal("revoked lease accepted")
	}
}
func TestSessionOperationCloseJoinsOutstandingRead(t *testing.T) {
	f := fixtureFor(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.inv.Reader = reader{get: func(_ context.Context, b string, q workbench.BacklogItemRequest) (workbench.BacklogItem, error) {
		close(entered)
		<-release
		return item(b, q.ID), nil
	}}
	access, closeAccess, err := f.bridge.Open(f.inv)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.bridge.GetBacklogItem(t.Context(), access.BearerToken, f.inv.Identity.RunID, readRequest())
	}()
	<-entered
	closed := make(chan struct{})
	go func() { closeAccess(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("close escaped active provider operation")
	default:
	}
	close(release)
	<-done
	<-closed
	if _, err = f.bridge.AuthenticateSessionOperation(access.BearerToken); err == nil {
		t.Fatal("closed grant accepted")
	}
}
func TestSessionOperationsAuditFailureBoundsAndSourceScope(t *testing.T) {
	for _, mode := range []string{"audit", "scope", "limit", "actor"} {
		t.Run(mode, func(t *testing.T) {
			f := fixtureFor(t)
			calls := 0
			f.inv.Reader = reader{get: func(_ context.Context, b string, q workbench.BacklogItemRequest) (workbench.BacklogItem, error) {
				calls++
				v := item(b, q.ID)
				if mode == "scope" {
					v.Ref.GaggleID = "other"
				}
				return v, nil
			}}
			if mode == "actor" {
				f.inv.Actor.Subject = "bob"
				if _, _, err := f.bridge.Open(f.inv); err == nil {
					t.Fatal("actor mismatch accepted")
				}
				return
			}
			access := open(t, f)
			if mode == "audit" {
				f.rec.fail = true
			}
			if mode == "limit" {
				g, _ := f.bridge.lookup(access.BearerToken)
				g.calls = sessioning.MaxOperationsPerTurn
			}
			if _, err := f.bridge.GetBacklogItem(t.Context(), access.BearerToken, f.inv.Identity.RunID, readRequest()); err == nil {
				t.Fatal("unsafe operation accepted")
			}
			if mode != "scope" && calls != 0 {
				t.Fatal("provider called past admission boundary")
			}
		})
	}
}

func TestSessionOperationLeaseCancellationInterruptsSourceRead(t *testing.T) {
	f := fixtureFor(t)
	entered := make(chan struct{})
	f.inv.Reader = reader{get: func(ctx context.Context, _ string, _ workbench.BacklogItemRequest) (workbench.BacklogItem, error) {
		close(entered)
		<-ctx.Done()
		return workbench.BacklogItem{}, ctx.Err()
	}}
	access := open(t, f)
	done := make(chan error, 1)
	go func() {
		_, err := f.bridge.GetBacklogItem(t.Context(), access.BearerToken, f.inv.Identity.RunID, readRequest())
		done <- err
	}()
	<-entered
	f.cancel()
	if err := <-done; err == nil {
		t.Fatal("source read survived revocation")
	}
	if len(f.rec.events) != 2 || f.rec.events[1].Runner["outcome"] != "failed" {
		t.Fatal("canceled source command lacks outcome evidence")
	}
}
