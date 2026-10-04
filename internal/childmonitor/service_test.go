package childmonitor

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestMonitorScopesPagesAndVerifiesRunLinks(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	parent := "0123456789abcdef0123456789abcdef"
	createMonitorRun(t, layout, journal.RunIdentity{RunID: parent, Gaggle: "own", Workflow: "parent", WorkflowVersion: 1})
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte("sensitive-invocation"))
	gaggles := []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "own"}}}
	permissions, err := interactiveaccess.New(gaggles, nil, interactiveaccess.Dependencies{Registrar: registry})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Layout: layout, Queue: queue, Permissions: permissions, Scrubber: registry}
	human := httpapi.Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleView}}
	var first triggerqueue.ChildRecord
	var firstEnvelope childworkflow.ChildStartEnvelope
	for i := range 51 {
		child, envelope := acceptMonitorChild(t, queue, "own", parent, fmt.Sprintf("occurrence-%d", i), "sensitive-invocation")
		if i == 0 {
			first, firstEnvelope = child, envelope
		} else {
			settleMonitorChild(t, queue, child)
		}
	}
	acceptMonitorChild(t, queue, "other", parent, "foreign", "foreign-key")
	page, err := service.ListChildWorkflows(t.Context(), human, parent, "")
	if err != nil || len(page.Children) != 50 || page.NextCursor == "" || page.Parent != nil {
		t.Fatal(page, err)
	}
	for _, child := range page.Children {
		if child.RunAvailable || child.InvocationKey == "sensitive-invocation" || child.InvocationKey == "foreign-key" || child.Workflow != "generated" || child.Stage != "plan" {
			t.Fatal("unsafe or cross-gaggle projection", child)
		}
	}
	last, err := service.ListChildWorkflows(t.Context(), human, parent, page.NextCursor)
	if err != nil || len(last.Children) != 1 || last.NextCursor != "" {
		t.Fatal(last, err)
	}
	start, err := queue.ChildStart(t.Context(), first.Identity)
	if err != nil {
		t.Fatal(err)
	}
	id := journal.RunIdentity{RunID: first.RunID, Gaggle: "own", Workflow: "generated", WorkflowVersion: 1, WorkflowDigest: firstEnvelope.WorkflowDigest, GooberDigest: firstEnvelope.ParentGooberDigest, ConfigGeneration: firstEnvelope.ConfigGeneration,
		Child: &journal.ChildLineage{Gaggle: "own", ParentRunID: parent, ParentWorkflow: "parent", StageOccurrence: first.Identity.StageOccurrence, InvocationKey: first.Identity.InvocationKey, AcceptanceID: first.AcceptanceID, SourceDigest: first.ProposalDigest, EnvelopeDigest: journal.Digest(start.Payload)}}
	createMonitorRun(t, layout, id)
	view, err := service.summarize(t.Context(), first)
	if err != nil || !view.RunAvailable {
		t.Fatal("published exact child run not linked", view, err)
	}
	childPage, err := service.ListChildWorkflows(t.Context(), human, first.RunID, "")
	if err != nil || childPage.Parent == nil || childPage.Parent.RunID != parent || childPage.Parent.InvocationKey == "sensitive-invocation" {
		t.Fatal(childPage, err)
	}
	// An explicit gaggle policy takes precedence over instance-wide view.
	gaggles[0].Spec.InteractiveAccess = &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Viewers: []apiv1.InteractiveHumanGrant{{Issuer: human.Issuer, Subject: "bob"}}}}
	if err = permissions.Apply(gaggles, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = service.ListChildWorkflows(t.Context(), human, parent, ""); err == nil {
		t.Fatal("explicit gaggle restriction ignored")
	}
	human.Subject = "bob"
	if _, err = service.ListChildWorkflows(t.Context(), human, parent, ""); err != nil {
		t.Fatal("configured viewer refused", err)
	}
	human.Issuer = httpapi.PodPrincipalIssuer
	if _, err = service.ListChildWorkflows(t.Context(), human, parent, ""); err == nil {
		t.Fatal("pod used human monitoring")
	}
}

func createMonitorRun(t *testing.T, layout instance.Layout, id journal.RunIdentity) {
	t.Helper()
	run, err := journal.Create(layout.ForGaggle(id.Gaggle).RunsDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
}

func acceptMonitorChild(t *testing.T, queue *triggerqueue.Store, gaggle, parent, occurrence, key string) (triggerqueue.ChildRecord, childworkflow.ChildStartEnvelope) {
	t.Helper()
	source := []byte("retained generated definition")
	digest := journal.Digest(source)
	e := childworkflow.ChildStartEnvelope{Kind: childworkflow.ChildStartKind, Version: 1, Gaggle: gaggle, ParentRunID: parent, ParentStage: "plan", ParentWorkflow: "parent", Workflow: "generated", StageOccurrence: occurrence, InvocationKey: key, ConfigGeneration: digest, ParentWorkflowDigest: digest, ParentGooberDigest: digest, SourceDigest: digest, CanonicalDigest: digest, ConfigDigest: digest, PolicyDigest: digest, WorkflowDigest: digest, PlacementsDigest: digest, Backend: childworkflow.BackendRunner, MaxChildren: 1}
	raw, _ := json.Marshal(e)
	child, _, err := queue.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: e.Identity(), Actor: "parent-stage", Payload: raw, MaxChildren: 1, Proposal: &triggerqueue.ChildProposal{Digest: digest, Source: source}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return child, e
}

// Retain completed history without reserving 51 concurrent execution carriers.
func settleMonitorChild(t *testing.T, queue *triggerqueue.Store, child triggerqueue.ChildRecord) {
	t.Helper()
	receipt := []byte(`{"status":"failed"}`)
	digest := journal.Digest(receipt)
	if err := queue.KeepChildResult(t.Context(), child, triggerqueue.ChildResult{Receipt: receipt, ReceiptDigest: digest}); err != nil {
		t.Fatal(err)
	}
	if err := queue.SetChildState(t.Context(), child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildFailed, ResultRef: digest}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := queue.AcknowledgeChild(t.Context(), child.Identity, digest, time.Now()); err != nil {
		t.Fatal(err)
	}
}
