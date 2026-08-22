package journal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
	sourcePath := filepath.Join(root, sourceID, fileEvents)
	before, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
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
	}
	continuation, err := CreateContinuation(root, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := continuation.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
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
