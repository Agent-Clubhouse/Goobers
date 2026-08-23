package journal

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
)

func TestCreateContinuationLinksTerminalRunAndPreservesSource(t *testing.T) {
	root := t.TempDir()
	sourceID := "0af7651916cd43dd8448eb211c80319c"
	continuationID := "1af7651916cd43dd8448eb211c80319c"
	source, err := Create(root, RunIdentity{
		RunID: sourceID, Workflow: "wf", WorkflowVersion: 2,
		WorkflowDigest: Digest([]byte("wf")), Gaggle: "g",
		Trigger: Trigger{Kind: TriggerManual},
	}, map[string][]byte{"original": []byte("source")})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Append(Event{Type: EventRunFinished, Status: string(PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(root, sourceID)
	before := snapshotJournalTree(t, sourceDir)
	sourceReader, err := OpenRead(filepath.Join(root, sourceID))
	if err != nil {
		t.Fatal(err)
	}
	sourceEvents, err := sourceReader.Events()
	if err != nil {
		t.Fatal(err)
	}
	req := ContinuationRequest{
		RunID: continuationID, SourceRunID: sourceID,
		ExpectedTerminalSeq: sourceEvents[len(sourceEvents)-1].Seq,
		Operator:            "operator@example.test", Target: "implement",
		Inputs: map[string][]byte{"injected": []byte("operator input")},
		InputIntegrity: map[string]apiv1.Integrity{
			"injected": apiv1.IntegrityMaintainer,
		},
		InputSource: map[string]string{"injected": "operator://payload"},
	}
	continuation, err := CreateContinuation(root, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := continuation.Close(); err != nil {
		t.Fatal(err)
	}
	validator, err := validate.New()
	if err != nil {
		t.Fatalf("build validator: %v", err)
	}
	runYAML, err := os.ReadFile(filepath.Join(root, continuationID, fileRunYAML))
	if err != nil {
		t.Fatal(err)
	}
	runJSON, err := yaml.YAMLToJSON(runYAML)
	if err != nil {
		t.Fatalf("continuation run.yaml to JSON: %v", err)
	}
	if err := validator.ValidateJSON("journal-run.schema.json", runJSON); err != nil {
		t.Fatalf("continuation run.yaml fails schema validation: %v\n%s", err, runJSON)
	}
	after := snapshotJournalTree(t, sourceDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatal("source journal changed while creating continuation")
	}

	reader, err := OpenRead(filepath.Join(root, continuationID))
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if id.ContinuedFromRunID != sourceID || id.SourceTerminalSeq != req.ExpectedTerminalSeq ||
		id.Operator != req.Operator || id.RequestedTarget != req.Target {
		t.Fatalf("continuation identity = %+v", id)
	}
	if len(id.Inputs) != 1 || id.Inputs[0].Name != "injected" ||
		id.Inputs[0].Source != req.InputSource["injected"] ||
		id.Inputs[0].Integrity != apiv1.IntegrityMaintainer ||
		!strings.HasPrefix(id.Inputs[0].Ref.Digest, "sha256:") {
		t.Fatalf("continuation input = %+v", id.Inputs)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if events[0].SourceRunID != sourceID || events[0].SourceTerminalSeq != req.ExpectedTerminalSeq ||
		events[0].Actor != req.Operator || events[0].Target != req.Target {
		t.Fatalf("continuation run.started = %+v", events[0])
	}
}

func TestCreateContinuationRejectsNonTerminalAndStaleSource(t *testing.T) {
	root := t.TempDir()
	sourceID := "0af7651916cd43dd8448eb211c80319c"
	source, err := Create(root, RunIdentity{RunID: sourceID, Workflow: "wf", Gaggle: "g"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	req := ContinuationRequest{
		RunID: "1af7651916cd43dd8448eb211c80319c", SourceRunID: sourceID,
		ExpectedTerminalSeq: 1, Operator: "operator", Target: "next",
	}
	if _, err := CreateContinuation(root, req); err == nil || !strings.Contains(err.Error(), "not terminal") {
		t.Fatalf("non-terminal error = %v", err)
	}

	// A second terminal event advances the source generation.
	writer, _, err := Recover(filepath.Join(root, sourceID))
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(Event{Type: EventRunFinished, Status: string(PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req.ExpectedTerminalSeq = 1
	if _, err := CreateContinuation(root, req); !errors.Is(err, ErrTerminalGenerationChanged) {
		t.Fatalf("stale error = %v, want ErrTerminalGenerationChanged", err)
	}
}

func TestCreateContinuationRejectsLegacySourceWithoutMutation(t *testing.T) {
	sourceDir := copyLegacyJournalFixture(t)
	root := filepath.Dir(sourceDir)
	before := snapshotJournalTree(t, sourceDir)
	restore := SetLockTimeoutForTest(100*time.Millisecond, 5*time.Millisecond)
	t.Cleanup(restore)

	continuationID := "1af7651916cd43dd8448eb211c80319c"
	_, err := CreateContinuation(root, ContinuationRequest{
		RunID:               continuationID,
		SourceRunID:         legacyFixtureRunID,
		ExpectedTerminalSeq: 2,
		Operator:            "operator@example.test",
		Target:              "implement",
	})
	if !errors.Is(err, ErrJournalMigrationRequired) {
		t.Fatalf("CreateContinuation error = %v, want ErrJournalMigrationRequired", err)
	}
	after := snapshotJournalTree(t, sourceDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("legacy source journal changed while refusing continuation:\nbefore: %v\nafter:  %v", before, after)
	}
	if _, statErr := os.Stat(filepath.Join(root, continuationID)); !os.IsNotExist(statErr) {
		t.Fatalf("continuation directory exists after legacy-source refusal: %v", statErr)
	}
}

func TestCreateContinuationRejectsMissingSourceLockWithoutMutation(t *testing.T) {
	root := t.TempDir()
	sourceID := "0af7651916cd43dd8448eb211c80319c"
	source, err := Create(root, RunIdentity{
		RunID: sourceID, Workflow: "wf", Gaggle: "g",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := source.Append(Event{Type: EventRunFinished, Status: string(PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	terminalSeq := source.Seq()
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(root, sourceID)
	if err := os.Remove(filepath.Join(sourceDir, fileLock)); err != nil {
		t.Fatal(err)
	}
	before := snapshotJournalTree(t, sourceDir)

	_, err = CreateContinuation(root, ContinuationRequest{
		RunID:               "1af7651916cd43dd8448eb211c80319c",
		SourceRunID:         sourceID,
		ExpectedTerminalSeq: terminalSeq,
		Operator:            "operator@example.test",
		Target:              "implement",
	})
	if !errors.Is(err, ErrImmutableSourceLockMissing) {
		t.Fatalf("CreateContinuation error = %v, want ErrImmutableSourceLockMissing", err)
	}
	after := snapshotJournalTree(t, sourceDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("source journal changed while refusing missing lock:\nbefore: %v\nafter:  %v", before, after)
	}
}

func TestCreateContinuationRetainsBranchAndExplicitContext(t *testing.T) {
	root := t.TempDir()
	sourceID := "0af7651916cd43dd8448eb211c80319c"
	source, err := Create(root, RunIdentity{RunID: sourceID, Workflow: "wf", Gaggle: "g"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Append(Event{Type: EventRefTouched, ExternalRef: &ExternalRef{
		Provider: "github", Kind: "branch", ID: "goobers/wf/source", CommitSHA: "abc123",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := source.Append(Event{Type: EventRunFinished, Status: string(PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	continuation, err := CreateContinuation(root, ContinuationRequest{
		RunID: "1af7651916cd43dd8448eb211c80319c", SourceRunID: sourceID,
		ExpectedTerminalSeq: 3, Operator: "operator", Target: "implement",
		Inputs:         map[string][]byte{"issue": []byte("body")},
		InputIntegrity: map[string]apiv1.Integrity{"issue": apiv1.IntegrityMaintainer},
		ContextPointers: []apiv1.ContextPointer{{
			Name: "prior.diff", RunID: sourceID,
			Artifact: &apiv1.ArtifactPointer{Path: "artifacts/review/diff", Digest: Digest([]byte("diff"))},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := continuation.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenRead(filepath.Join(root, "1af7651916cd43dd8448eb211c80319c"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if id.WorkspaceBranch != "goobers/wf/source" || id.WorkspaceBranchSHA != "abc123" {
		t.Fatalf("continuation branch = %q@%q", id.WorkspaceBranch, id.WorkspaceBranchSHA)
	}
	if len(id.ContextPointers) != 2 || id.ContextPointers[0].Name != "prior.diff" ||
		id.ContextPointers[1].Name != "issue" || id.ContextPointers[1].Artifact.Path != "inputs/issue" {
		t.Fatalf("continuation context = %+v", id.ContextPointers)
	}
}

func TestCreateContinuationRejectsMismatchedSourceCommit(t *testing.T) {
	root := t.TempDir()
	sourceID := "0af7651916cd43dd8448eb211c80319c"
	source, err := Create(root, RunIdentity{RunID: sourceID, Workflow: "wf", Gaggle: "g"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Append(Event{Type: EventRefTouched, ExternalRef: &ExternalRef{
		Kind: "branch", ID: "goobers/wf/source", CommitSHA: "actual",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := source.Append(Event{Type: EventRunFinished, Status: string(PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = CreateContinuation(root, ContinuationRequest{
		RunID: "1af7651916cd43dd8448eb211c80319c", SourceRunID: sourceID,
		ExpectedTerminalSeq: 3, Operator: "operator", Target: "implement",
		SourceBranch: "goobers/wf/source", ExpectedSourceSHA: "stale",
	})
	if err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("mismatched commit error = %v", err)
	}
}

func TestCreateContinuationDoesNotInheritSourceContext(t *testing.T) {
	root := t.TempDir()
	sourceID := "0af7651916cd43dd8448eb211c80319c"
	source, err := Create(root, RunIdentity{
		RunID: sourceID, Workflow: "wf", Gaggle: "g",
		ContextPointers: []apiv1.ContextPointer{{
			Name: "ambient", Artifact: &apiv1.ArtifactPointer{
				Path: "artifacts/ambient", Digest: Digest([]byte("ambient")),
			},
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Append(Event{Type: EventRunFinished, Status: string(PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	continuation, err := CreateContinuation(root, ContinuationRequest{
		RunID: "1af7651916cd43dd8448eb211c80319c", SourceRunID: sourceID,
		ExpectedTerminalSeq: 2, Operator: "operator", Target: "implement",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := continuation.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenRead(filepath.Join(root, "1af7651916cd43dd8448eb211c80319c"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if len(id.ContextPointers) != 0 {
		t.Fatalf("continuation context = %+v, want no inherited pointers", id.ContextPointers)
	}
}

func snapshotJournalTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			snapshot[relative+string(filepath.Separator)] = ""
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[relative] = string(data)
		return nil
	}); err != nil {
		t.Fatalf("snapshot journal tree: %v", err)
	}
	return snapshot
}
