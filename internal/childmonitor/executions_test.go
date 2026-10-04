package childmonitor

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpublication"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

func TestMonitorCurrentChildExecutionAndImmutablePublicationOrigin(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	parent := strings.Repeat("a", 32)
	createMonitorRun(t, layout, journal.RunIdentity{RunID: parent, Gaggle: "own", Workflow: "parent", WorkflowVersion: 1})
	child, envelope := acceptMonitorChild(t, queue, "own", parent, "work/1", "inspect")
	start, err := queue.ChildStart(t.Context(), child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	id := journal.RunIdentity{RunID: child.RunID, Gaggle: "own", Workflow: envelope.Workflow, WorkflowVersion: 1, WorkflowDigest: envelope.WorkflowDigest, GooberDigest: envelope.ParentGooberDigest, ConfigGeneration: envelope.ConfigGeneration, Child: &journal.ChildLineage{Gaggle: "own", ParentRunID: parent, ParentWorkflow: envelope.ParentWorkflow, StageOccurrence: child.Identity.StageOccurrence, InvocationKey: child.Identity.InvocationKey, AcceptanceID: child.AcceptanceID, SourceDigest: child.ProposalDigest, EnvelopeDigest: journal.Digest(start.Payload)}}
	writer, err := journal.Create(layout.ForGaggle("own").RunsDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Append(journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(writer.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	intentRaw, _ := json.Marshal(childpublication.BranchIntent{Version: 1, RunID: child.RunID, Lineage: *id.Child, Stage: "push", Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}, Base: "main", Head: "goobers/children/" + child.RunID, Commit: strings.Repeat("b", 40)})
	intent, err := queue.PrepareChildPublication(t.Context(), child.Identity, "branch", intentRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err = queue.BeginChildPublicationEffect(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	result := triggerqueue.ChildResult{Receipt: []byte("sealed failed result")}
	result.ReceiptDigest = journal.Digest(result.Receipt)
	if err = queue.KeepChildResult(t.Context(), child, result); err != nil {
		t.Fatal(err)
	}
	if err = queue.SetChildState(t.Context(), child.Identity, triggerqueue.ChildStateUpdate{Expected: child.State, State: triggerqueue.ChildFailed, ResultRef: result.ReceiptDigest}, time.Now()); err != nil {
		t.Fatal(err)
	}
	plan := []byte("private restart plan and selected instructions")
	epoch, _, err := queue.BeginChildRestart(t.Context(), triggerqueue.ChildRestartRequest{Identity: child.Identity, RunID: strings.Repeat("c", 32), SourceRunID: child.RunID, SourceTerminalSeq: events[len(events)-1].Seq, SourceResultRef: result.ReceiptDigest, Actor: "private-human", Stage: "repair", Plan: plan, PlanDigest: journal.Digest(plan)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	scrubber := journal.NewRegistryScrubber()
	scrubber.Register([]byte("private-human"))
	policy, err := interactiveaccess.New([]apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "own"}}}, nil, interactiveaccess.Dependencies{Registrar: scrubber})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Layout: layout, Queue: queue, Permissions: policy, Scrubber: scrubber}
	human := httpapi.Principal{Issuer: "issuer", Subject: "viewer", Roles: []httpapi.Role{httpapi.RoleView}}
	page, err := service.ListChildWorkflows(t.Context(), human, parent, "")
	if err != nil || len(page.Children) != 1 || page.Children[0].RunID != epoch.RunID || page.Children[0].OriginalRunID != child.RunID || page.Children[0].ExecutionEpoch != 1 || page.Children[0].RunAvailable {
		t.Fatal("unpublished current execution was mislinked", page, err)
	}
	lineage := *id.Child
	lineage.ExecutionEpoch, lineage.PriorResultRef, lineage.RestartDigest = epoch.Epoch, epoch.SourceResultRef, epoch.RequestDigest
	next, err := journal.CreateContinuation(layout.ForGaggle("own").RunsDir(), journal.ContinuationRequest{RunID: epoch.RunID, SourceRunID: child.RunID, ExpectedTerminalSeq: epoch.SourceTerminalSeq, Operator: epoch.Actor, Target: epoch.Stage, ChildContinuation: &lineage})
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Close(); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{child.RunID, epoch.RunID} {
		page, err = service.ListChildWorkflows(t.Context(), human, run, "")
		if err != nil || len(page.ExecutionHistory) != 2 || page.ExecutionHistory[0].Current || !page.ExecutionHistory[1].Current || !page.ExecutionHistory[1].RunAvailable || page.PublicationRunID != child.RunID || len(page.Publications) != 1 {
			t.Fatal("history or original publication changed", page, err)
		}
		raw, _ := json.Marshal(page)
		if strings.Contains(string(raw), "private-human") || strings.Contains(string(raw), string(plan)) {
			t.Fatal("monitor exposed private actor or plan payload")
		}
	}
	page, err = service.ListChildWorkflows(t.Context(), human, parent, "")
	if err != nil || !page.Children[0].RunAvailable {
		t.Fatal("current published execution absent", page, err)
	}
	changed, err := service.identity(epoch.RunID)
	if err != nil {
		t.Fatal(err)
	}
	changed.Child.RestartDigest = journal.Digest([]byte("other plan"))
	if _, err = service.publicationChild(t.Context(), changed); err == nil {
		t.Fatal("changed execution lineage accepted")
	}
}
