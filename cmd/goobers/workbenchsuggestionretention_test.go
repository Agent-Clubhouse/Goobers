package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchsuggestions"
)

func TestWorkbenchSuggestionProductionJournalAndGenerationPins(t *testing.T) {
	layout := instance.NewLayout(initDeterministicDemo(t))
	if err := os.MkdirAll(layout.SchedulerDir(), 0700); err != nil {
		t.Fatal(err)
	}
	id := journal.RunIdentity{RunID: strings.Repeat("a", 32), Gaggle: "example", Workflow: "curate", WorkflowVersion: 1, ConfigGeneration: strings.Repeat("f", 64)}
	run, err := journal.Create(layout.ForGaggle(id.Gaggle).RunsDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	if err = run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "curate", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	set, raw := suggestionRetentionArtifact(t)
	if _, err = run.RecordStageArtifact("curate", 1, "", "suggestions.json", raw); err != nil {
		t.Fatal(err)
	}
	if err = run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(layout.ForGaggle(id.Gaggle).RunsDir(), id.RunID)
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := workbenchsuggestions.List(t.Context(), reader, id.Gaggle, id.RunID, 0)
	if err != nil || len(list.Artifacts) != 1 {
		t.Fatal(list, err)
	}
	loaded, err := workbenchsuggestions.Load(t.Context(), reader, set, workbenchsuggestions.Selection{RunID: id.RunID, Sequence: list.Artifacts[0].Sequence})
	if err != nil {
		t.Fatal(err)
	}
	service := acceptedService(t, filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"), newDaemonTriggerService())
	now := time.Now().UTC()
	in := triggerqueue.WorkbenchSuggestionInput{Scope: triggerqueue.WorkbenchSuggestionScope{Gaggle: id.Gaggle, Actor: sessioning.Actor{Issuer: "issuer", Subject: "human"}}, Suggestion: loaded.Suggestions[0], ConfigGeneration: loaded.ConfigGeneration, ArtifactSequence: loaded.Artifact.Sequence, StageSequence: loaded.Artifact.StageSequence, Branch: loaded.Artifact.Branch, Decision: "reject", Reason: "Not useful."}
	if _, _, err = service.queue.AcceptWorkbenchSuggestion(t.Context(), in, now); err != nil {
		t.Fatal(err)
	}
	guard, closeGuard, err := openTriggerPruneGuard(layout, false, now)
	if err != nil {
		t.Fatal(err)
	}
	defer closeGuard()
	candidate := retention.Result{RunID: id.RunID, RunDir: dir}
	if err = guard(candidate); !errors.Is(err, retention.ErrCustodyHeld) {
		t.Fatal("review lost producer journal", err)
	}
	pins := map[string]bool{}
	if err = retainEventGenerationPins(t.Context(), layout, pins); err != nil || !pins[id.ConfigGeneration] {
		t.Fatal(pins, err)
	}
	if n, err := service.queue.PruneWorkbenchCommands(t.Context(), now.Add(triggerqueue.WorkbenchCommandRetention), 100); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if err = guard(candidate); err != nil {
		t.Fatal("terminal review retained forever", err)
	}
	pins = map[string]bool{}
	if err = retainEventGenerationPins(t.Context(), layout, pins); err != nil || pins[id.ConfigGeneration] {
		t.Fatal(pins, err)
	}
}

func suggestionRetentionArtifact(t *testing.T) (workbench.SourceSet, []byte) {
	t.Helper()
	target := apiv1.InteractiveRepositoryIdentity{Provider: "github", Owner: "org", Name: "repo"}
	source := workbench.BoundSource{Spec: apiv1.WorkbenchSource{Name: "strategy", Kind: "documents", Repository: &target, Paths: []string{"plan.md"}}, Repository: apiv1.RepoRef{Provider: "github", Owner: "org", Name: "repo", Branch: "main"}}
	set := workbench.SourceSet{Scope: workbench.Scope{GaggleID: "example", Bindings: map[string]bool{"strategy": true}}, Sources: []workbench.BoundSource{source}}
	digest, err := workbench.SourceTargetDigest(set.Scope, source)
	if err != nil {
		t.Fatal(err)
	}
	from := workbench.NodeRef{GaggleID: "example", SourceBindingID: "strategy", Kind: "objective-document", SourceID: "obj-11111111-1111-4111-8111-111111111111"}
	to := from
	to.SourceID = "obj-22222222-2222-4222-8222-222222222222"
	evidence := &workbench.SuggestionEvidence{SourceTargetDigest: digest, Path: "plan.md", RepositoryRevision: &workbench.SuggestionRepositoryRevision{Commit: strings.Repeat("a", 40), BlobID: strings.Repeat("b", 40), ContentDigest: strings.Repeat("c", 64)}}
	raw, err := json.Marshal(workbench.RelationshipSuggestions{SchemaVersion: workbench.SuggestionSchemaVersion, Suggestions: []workbench.RelationshipSuggestion{{Kind: "references", From: workbench.SuggestionEndpoint{Ref: &from, Evidence: evidence}, To: workbench.SuggestionEndpoint{Ref: &to, Evidence: evidence}, Rationale: "Related plan."}}})
	if err != nil {
		t.Fatal(err)
	}
	return set, raw
}
