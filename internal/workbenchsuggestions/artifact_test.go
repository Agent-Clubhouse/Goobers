package workbenchsuggestions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workbench"
)

func fixture(t *testing.T) (*journal.Run, *journal.Reader, workbench.SourceSet, []byte) {
	t.Helper()
	id := journal.RunIdentity{RunID: strings.Repeat("a", 32), Workflow: "curate", WorkflowVersion: 1, Gaggle: "team", WorkflowDigest: journal.Digest([]byte("workflow")), GooberDigest: journal.Digest([]byte("goober")), ConfigGeneration: strings.Repeat("f", 64), Trigger: journal.Trigger{Kind: journal.TriggerManual}}
	root := t.TempDir()
	run, err := journal.Create(root, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	reader, err := journal.OpenRead(filepath.Join(root, id.RunID))
	if err != nil {
		t.Fatal(err)
	}
	target := apiv1.InteractiveRepositoryIdentity{Provider: "github", Owner: "acme", Name: "strategy"}
	source := workbench.BoundSource{Spec: apiv1.WorkbenchSource{Name: "strategy", Kind: "documents", Repository: &target, Paths: []string{"plan.md"}}, Repository: apiv1.RepoRef{Provider: "github", Owner: "acme", Name: "strategy", Branch: "main"}}
	set := workbench.SourceSet{Scope: workbench.Scope{GaggleID: "team", Bindings: map[string]bool{"strategy": true}}, Sources: []workbench.BoundSource{source}}
	digest, err := workbench.SourceTargetDigest(set.Scope, source)
	if err != nil {
		t.Fatal(err)
	}
	evidence := &workbench.SuggestionEvidence{SourceTargetDigest: digest, Path: "plan.md", RepositoryRevision: &workbench.SuggestionRepositoryRevision{Commit: strings.Repeat("a", 40), BlobID: strings.Repeat("b", 40), ContentDigest: strings.Repeat("c", 64)}}
	from := workbench.NodeRef{GaggleID: "team", SourceBindingID: "strategy", Kind: "objective-document", SourceID: "obj-11111111-1111-1111-1111-111111111111"}
	to := from
	to.SourceID = "obj-22222222-2222-2222-2222-222222222222"
	suggestion := workbench.RelationshipSuggestion{Kind: "references", From: workbench.SuggestionEndpoint{Ref: &from, Evidence: evidence}, To: workbench.SuggestionEndpoint{Ref: &to, Evidence: evidence}, Rationale: "Human should review this relationship."}
	raw, err := json.Marshal(workbench.RelationshipSuggestions{SchemaVersion: workbench.SuggestionSchemaVersion, Suggestions: []workbench.RelationshipSuggestion{suggestion, suggestion}})
	if err != nil {
		t.Fatal(err)
	}
	return run, reader, set, raw
}
func record(t *testing.T, run *journal.Run, stage string, attempt int, raw []byte) journal.Ref {
	t.Helper()
	ref, err := run.RecordStageArtifact(stage, attempt, "", "relationship-suggestions.json", raw)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
func started(t *testing.T, run *journal.Run, stage string, attempt int) {
	t.Helper()
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: stage, Attempt: attempt}); err != nil {
		t.Fatal(err)
	}
}
func TestSuggestionArtifactBindsActualCommittedOriginAndBytes(t *testing.T) {
	run, reader, set, raw := fixture(t)
	started(t, run, "curate", 1)
	ref := record(t, run, "curate", 1, raw)
	inventory, err := List(t.Context(), reader, "team", strings.Repeat("a", 32), 0)
	if err != nil || len(inventory.Artifacts) != 1 {
		t.Fatal(inventory, err)
	}
	chosen := inventory.Artifacts[0]
	loaded, err := Load(t.Context(), reader, set, Selection{RunID: strings.Repeat("a", 32), Sequence: chosen.Sequence})
	if err != nil || len(loaded.Suggestions) != 1 {
		t.Fatal(loaded, err)
	}
	if loaded.Origin.RunID != strings.Repeat("a", 32) || loaded.Origin.StageID != "curate" || loaded.Origin.Attempt != 1 || loaded.Origin.ArtifactPath != ref.Path || loaded.Origin.ArtifactDigest != strings.TrimPrefix(ref.Digest, "sha256:") || loaded.ConfigGeneration != strings.Repeat("f", 64) || loaded.Artifact.StageSequence == 0 {
		t.Fatal("unbound origin", loaded)
	}
	if _, err := Load(t.Context(), reader, set, Selection{RunID: strings.Repeat("a", 32), Sequence: chosen.StageSequence}); err == nil {
		t.Fatal("accepted stage as artifact")
	}
	if _, err := List(t.Context(), reader, "foreign", strings.Repeat("a", 32), 0); err == nil {
		t.Fatal("foreign gaggle")
	}
	if _, err := Load(t.Context(), reader, set, Selection{RunID: strings.Repeat("b", 32), Sequence: chosen.Sequence}); err == nil {
		t.Fatal("caller run override")
	}
	if err := os.WriteFile(filepath.Join(reader.Dir(), ref.Path), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(t.Context(), reader, set, Selection{RunID: strings.Repeat("a", 32), Sequence: chosen.Sequence}); err == nil {
		t.Fatal("tampered bytes accepted")
	}
}
func TestSuggestionArtifactRequiresUnambiguousActualAttemptAndClosedProducerData(t *testing.T) {
	run, reader, set, raw := fixture(t)
	record(t, run, "unstarted", 1, raw)
	started(t, run, "curate", 1)
	started(t, run, "curate", 1)
	record(t, run, "curate", 1, raw)
	started(t, run, "valid", 2)
	injected := strings.TrimSuffix(string(raw), "}") + `,"origin":{"runId":"forged"}}`
	record(t, run, "valid", 2, []byte(injected))
	inventory, err := List(t.Context(), reader, "team", strings.Repeat("a", 32), 0)
	if err != nil || len(inventory.Artifacts) != 1 {
		t.Fatal(inventory, err)
	}
	if _, err := Load(t.Context(), reader, set, Selection{RunID: strings.Repeat("a", 32), Sequence: inventory.Artifacts[0].Sequence}); err == nil {
		t.Fatal("model asserted provenance")
	}
}
func TestSuggestionArtifactWindowsDoNotReadBodiesOrLoseContinuation(t *testing.T) {
	run, reader, _, raw := fixture(t)
	started(t, run, "curate", 1)
	for range MaxArtifactWindow + 1 {
		record(t, run, "curate", 1, raw)
	}
	first, err := List(t.Context(), reader, "team", strings.Repeat("a", 32), 0)
	if err != nil || len(first.Artifacts) != MaxArtifactWindow || !first.Partial || first.NextSequence == 0 {
		t.Fatal(first, err)
	}
	second, err := List(t.Context(), reader, "team", strings.Repeat("a", 32), first.NextSequence)
	if err != nil || len(second.Artifacts) != 1 || second.Partial {
		t.Fatal(second, err)
	}
}
