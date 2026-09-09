package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/investigation"
	"github.com/goobers/goobers/internal/journal"
)

type evidenceRecorder struct {
	run    *journal.Run
	writes int
}

func (r *evidenceRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	r.writes++
	return r.run.RecordArtifact(name, data)
}

func TestExecutorPublishesInvestigationFromSemanticDraft(t *testing.T) {
	for _, lateInvalid := range []bool{false, true} {
		t.Run(fmt.Sprintf("late-invalid-%v", lateInvalid), func(t *testing.T) {
			runs := t.TempDir()
			run, err := journal.Create(runs, journal.RunIdentity{RunID: "run-1", Workflow: "investigation", WorkflowVersion: 1, Gaggle: "example", Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = run.Close() })
			draft, contexts, expected := executorEvidenceFixture(t, run)
			adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
				if _, err := os.Stat(filepath.Join(req.Workspace, ".git")); !os.IsNotExist(err) {
					return errors.New("evidence packager workspace is not verifiably checkout-free")
				}
				entries := []artifactset.ManifestEntry{{Name: "investigation.evidence", Path: "draft.json", MediaType: "application/json"}}
				if lateInvalid {
					entries = append(entries, artifactset.ManifestEntry{Name: "z.invalid", Path: "draft.json", MediaType: "unsupported"})
				}
				manifest, err := json.Marshal(artifactset.Manifest{SchemaVersion: artifactset.SchemaVersion, Entries: entries})
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(req.Workspace, "draft.json"), draft, 0o600); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(req.Workspace, "manifest.json"), manifest, 0o600); err != nil {
					return err
				}
				return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			}}
			recorder := &evidenceRecorder{run: run}
			e, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), run, recorder, NewContextResolver(run, runs), journal.NewRegistryScrubber(), "")
			if err != nil {
				t.Fatal(err)
			}
			env := testEnvelope(t.TempDir())
			env.TaskID = "emit-evidence"
			env.ContextPointers = contexts
			env.Inputs = map[string]interface{}{InputArtifactManifestFile: "manifest.json"}
			result, err := e.Invoke(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			if lateInvalid {
				if result.Status != apiv1.ResultFailure || recorder.writes != 0 || len(result.Artifacts) != 0 {
					t.Fatalf("later invalid entry published earlier evidence: %+v, writes=%d", result, recorder.writes)
				}
				return
			}
			if result.Status != apiv1.ResultSuccess || recorder.writes != 2 || len(result.Artifacts) != 2 {
				t.Fatalf("evidence emission = %+v, writes=%d", result, recorder.writes)
			}
			var output []apiv1.ContextPointer
			for i := range result.Artifacts {
				output = append(output, apiv1.ContextPointer{Name: fmt.Sprintf("emit-evidence.artifact[%d]", i), Artifact: &result.Artifacts[i]})
			}
			reader, err := artifactset.OpenJournal(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reader.Close() })
			payloads, err := artifactset.Resolve(context.Background(), reader, output, "emit-evidence", "investigation.evidence")
			if err != nil {
				t.Fatal(err)
			}
			var got investigation.Evidence
			if err := json.Unmarshal(payloads["investigation.evidence"].Bytes, &got); err != nil {
				t.Fatal(err)
			}
			if got.SchemaVersion != investigation.SchemaVersion || got.Reproduction.Harness != expected["reproduce/harness"] || got.Reproduction.Baseline != expected["reproduce/baseline"] || got.Diagnosis.Report != expected["instrument/report"] || got.Diagnosis.Evidence[0].Artifact != expected["instrument/support"] || got.Fix.Report != expected["implement-fix/report"] || got.Validation.Result != expected["validate-reproduction/result"] {
				t.Fatalf("runner-authored canonical evidence differs: %+v", got)
			}
		})
	}
}

func executorEvidenceFixture(t *testing.T, run *journal.Run) ([]byte, []apiv1.ContextPointer, map[string]apiv1.ArtifactPointer) {
	t.Helper()
	var contexts []apiv1.ContextPointer
	expected := map[string]apiv1.ArtifactPointer{}
	for stage, names := range map[string][]string{"reproduce": {"baseline", "harness"}, "instrument": {"report", "support"}, "implement-fix": {"report"}, "validate-reproduction": {"result"}} {
		index := artifactset.Index{SchemaVersion: artifactset.SchemaVersion}
		for i, name := range names {
			ref, err := run.RecordArtifact(stage+"/"+name, []byte(stage+"/"+name))
			if err != nil {
				t.Fatal(err)
			}
			p := refToPointer(ref, "text/plain")
			expected[stage+"/"+name] = p
			index.Entries = append(index.Entries, artifactset.Entry{Name: name, Slot: i + 1, Artifact: p})
			contexts = append(contexts, apiv1.ContextPointer{Name: fmt.Sprintf("%s.artifact[%d]", stage, i+1), Artifact: &p})
		}
		data, err := json.Marshal(index)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := run.RecordArtifact(stage+"/index", data)
		if err != nil {
			t.Fatal(err)
		}
		p := refToPointer(ref, "application/json")
		contexts = append(contexts, apiv1.ContextPointer{Name: stage + ".artifact[0]", Artifact: &p})
	}
	ref := func(stage, name string) map[string]string {
		return map[string]string{"producerStage": stage, "name": name}
	}
	digest := apiv1.Digest([]byte("config"))
	draft := map[string]any{
		"schemaVersion": investigation.DraftSchemaVersion,
		"subject":       investigation.Subject{Item: apiv1.ExternalRef{Kind: "issue", URI: "https://example.com/issues/1", Description: "investigate"}, Repository: "owner/repo", BaseRevision: strings.Repeat("a", 40), FixRevision: strings.Repeat("b", 40)},
		"environment":   investigation.Environment{Platform: "linux/amd64", Dimensions: map[string]any{"workers": 4}, ConfigDigest: digest},
		"reproduction":  map[string]any{"harness": ref("reproduce", "harness"), "baseline": ref("reproduce", "baseline"), "sourceSnapshotDigest": digest, "oracleDigest": digest, "symptom": "stall"},
		"diagnosis":     map[string]any{"report": ref("instrument", "report"), "confidence": "confirmed", "evidence": []any{map[string]any{"kind": "log", "artifact": ref("instrument", "support"), "producerStage": "instrument", "description": "causal evidence", "captureContext": map[string]any{"workers": 4}}}},
		"fix":           map[string]any{"report": ref("implement-fix", "report"), "diffDigest": digest},
		"validation":    map[string]any{"result": ref("validate-reproduction", "result"), "sourceSnapshotDigest": digest, "harnessDigest": expected["reproduce/harness"].Digest, "oracleDigest": digest, "completedAttempts": 3, "symptomObservationsBefore": 1, "symptomObservationsAfter": 0, "passed": true},
	}
	data, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	return data, contexts, expected
}
