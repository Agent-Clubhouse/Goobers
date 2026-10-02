package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/launchreceipt"
	"github.com/goobers/goobers/internal/livejournal"
	wf "github.com/goobers/goobers/internal/workflow"
)

type attestationResolver struct{ root string }

func (r attestationResolver) ResolveReceiptStore(context.Context, launchreceipt.Binding) (launchreceipt.RuntimeStore, error) {
	return launchreceipt.RuntimeStore{Root: r.root, Kind: launchreceipt.LocalStore}, nil
}

func TestStageAttemptAttestationDurableProjectionAndLiveParity(t *testing.T) {
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "review")}, []apiv1.Gate{crGate("review", map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort})})
	proj := executeForProjection(t, projectionInput("attestation-projection", spec), &Activities{Det: &scriptedStages{}, Auto: gate.NewAutomatedEvaluator(), Workspaces: testWorkspaces(t)}, false)
	root := t.TempDir()
	reader := launchreceipt.Reader{Runtime: attestationResolver{root}}
	expected := make(map[string]launchreceipt.Projection)
	for _, branch := range []int{0, 2} {
		binding := launchreceipt.Binding{RunID: proj.Identity.RunID, Stage: "implement", Branch: branch, StartedSeq: 2, Number: 1}
		binding.AttemptID = journal.StageAttemptID(binding.RunID, branch, binding.Stage, binding.StartedSeq)
		facts := launchreceipt.PreparedLocal("agent")
		receipt := launchreceipt.Receipt{Version: 1, Binding: binding, Local: &facts}
		if err := (launchreceipt.LocalRecorder{Root: root}).Record(context.Background(), receipt); err != nil {
			t.Fatal(err)
		}
		assembled, err := reader.Assemble(context.Background(), binding, launchreceipt.MaxBytes)
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := StageAttemptAttestationArtifact(assembled)
		if err != nil {
			t.Fatal(err)
		}
		expected[artifact.Name] = assembled
		op := JournalOp{Kind: opArtifact, Artifact: &artifact, Time: proj.Ops[len(proj.Ops)-1].Time, EmitKey: assembled.Reference().Name}
		last := proj.Ops[len(proj.Ops)-1]
		proj.Ops[len(proj.Ops)-1] = op
		proj.Ops = append(proj.Ops, last)
		live := liveOpFrom(op)
		if live.Artifact.Branch != branch || !bytes.Equal(live.Artifact.Data, assembled.Bytes()) {
			t.Fatal("live conversion lost identity/bytes")
		}
	}
	// Serialize the exact existing history shape, then discard the assembly
	// values; repair uses the recorded Data without touching a runtime store.
	raw, err := json.Marshal(proj)
	if err != nil {
		t.Fatal(err)
	}
	var restored JournalProjection
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	var first map[string][]byte
	for i := 0; i < 2; i++ {
		runsDir := t.TempDir()
		dir, err := ProjectRun(runsDir, restored)
		if err != nil {
			t.Fatal(err)
		}
		same, err := ProjectRun(runsDir, restored)
		if err != nil || same != dir {
			t.Fatalf("idempotent projection: %s %v", same, err)
		}
		files := readDirBytes(t, dir)
		if i == 0 {
			first = files
		} else if !reflect.DeepEqual(first, files) {
			t.Fatal("reprojection changed durable files")
		}
		assertAttestationArtifacts(t, dir, expected)
	}
	// The same adapter goes through the existing live journal writer, including
	// emit-key retry and restart deduplication, with full branch attribution.
	liveDir := t.TempDir()
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return liveDir, true })
	if err != nil {
		t.Fatal(err)
	}
	req := livejournal.EmitRequest{RunID: proj.Identity.RunID, Gaggle: proj.Identity.Gaggle, Open: &livejournal.OpenHeader{Identity: proj.Identity, Graph: proj.Graph, Definition: proj.Definition}}
	opening := liveOpFrom(restored.Ops[0])
	opening.Key = "attestation-open"
	req.Ops = append(req.Ops, opening)
	for _, op := range restored.Ops {
		if op.Kind == opArtifact && op.Artifact != nil {
			if _, ok := expected[op.Artifact.Name]; ok {
				req.Ops = append(req.Ops, liveOpFrom(op))
			}
		}
	}
	if _, err := writer.Emit(context.Background(), req); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	writer.Close()
	writer, err = livejournal.NewWriter(func(string) (string, bool) { return liveDir, true })
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	out, err := writer.Emit(context.Background(), req)
	if err != nil || out.Applied != 0 {
		t.Fatalf("restart duplicate: %+v %v", out, err)
	}
	assertAttestationArtifacts(t, filepath.Join(liveDir, proj.Identity.RunID), expected)
}

func assertAttestationArtifacts(t *testing.T, dir string, expected map[string]launchreceipt.Projection) {
	t.Helper()
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		p, ok := expected[event.Name]
		if !ok || event.Type != journal.EventArtifactRecorded {
			continue
		}
		count++
		ref, b := p.Reference(), p.Binding()
		if event.Ref == nil || event.Ref.Digest != ref.Digest || event.Ref.Size != ref.Size || event.Branch != b.Branch || event.Stage != b.Stage || event.Attempt != b.Number {
			t.Fatalf("durable ref/binding changed: %+v", event)
		}
		raw, err := rd.ArtifactBytesBounded(*event.Ref, launchreceipt.MaxBytes)
		if err != nil || !bytes.Equal(raw, p.Bytes()) {
			t.Fatalf("durable bytes changed: %v", err)
		}
	}
	if count != len(expected) {
		t.Fatalf("got %d artifacts, want %d", count, len(expected))
	}
}

func TestStageAttemptAttestationRejectsUnassembledProjection(t *testing.T) {
	var forged launchreceipt.Projection
	if err := json.Unmarshal([]byte(`{"binding":{"runId":"fake"},"raw":"forged","fidelity":"authenticated"}`), &forged); err != nil {
		t.Fatal(err)
	}
	if _, err := StageAttemptAttestationArtifact(forged); err == nil {
		t.Fatal("empty forged projection accepted")
	}
	raw, err := json.Marshal(JournalArtifactOp{Name: "legacy", Data: []byte("old")})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("branch")) {
		t.Fatalf("changed branch-zero history: %s", raw)
	}
}
