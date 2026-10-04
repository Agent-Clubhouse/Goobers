package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/investigation"
	"github.com/goobers/goobers/internal/journal"
)

func TestExecutorManifestArtifactSet(t *testing.T) {
	for _, scenario := range []string{"valid", "empty", "missing", "duplicate", "self-reported", "legacy", "invalid-type", "canonical-pointers", "invalid-draft"} {
		t.Run(scenario, func(t *testing.T) {
			rec := &fakeRecorder{}
			scrubber := journal.NewRegistryScrubber()
			scrubber.Register([]byte("test-secret-material"))
			adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
				entries := []artifactset.ManifestEntry{{Name: "z.report", Path: "payload", MediaType: "text/plain"}, {Name: "a.report", Path: "payload", MediaType: "text/plain"}}
				payload := []byte("test-secret-material evidence")
				if scenario == "canonical-pointers" || scenario == "invalid-draft" {
					version := investigation.SchemaVersion
					if scenario == "invalid-draft" {
						version = investigation.DraftSchemaVersion
					}
					payload = []byte(`{"schemaVersion":"` + version + `"}`)
					entries[0].MediaType = "application/json"
				}
				if scenario == "empty" {
					entries = []artifactset.ManifestEntry{}
				}
				if scenario == "duplicate" {
					entries[1].Name = entries[0].Name
				}
				if scenario == "missing" {
					entries[1].Path = "missing"
				}
				manifest, err := json.Marshal(artifactset.Manifest{SchemaVersion: artifactset.SchemaVersion, Entries: entries})
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(req.Workspace, "manifest.json"), manifest, 0o600); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(req.Workspace, "payload"), payload, 0o600); err != nil {
					return err
				}
				result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}
				if scenario == "self-reported" {
					result.Artifacts = []apiv1.ArtifactPointer{{Path: "artifacts/forged", Digest: apiv1.Digest(nil)}}
				}
				return WriteCompletion(req.Workspace, req.CompletionPath, result)
			}}
			e, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, scrubber, "")
			if err != nil {
				t.Fatal(err)
			}
			env := testEnvelope(t.TempDir())
			env.Inputs = map[string]interface{}{InputArtifactManifestFile: "manifest.json"}
			if scenario == "legacy" {
				env.Inputs[InputArtifactFile] = "payload"
			}
			if scenario == "invalid-type" {
				env.Inputs[InputArtifactManifestFile] = true
			}
			result, err := e.Invoke(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "valid" && scenario != "empty" {
				if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "invalid_declared_artifact_set" || len(result.Artifacts) != 0 || len(rec.artifacts) != 0 {
					t.Fatalf("invalid set published or wrong failure: %+v; records %d", result, len(rec.artifacts))
				}
				if !result.Error.Retryable {
					t.Fatalf("invalid declared artifact set was non-retryable: %+v", result.Error)
				}
				return
			}
			count := 3
			if scenario == "empty" {
				count = 1
			}
			if result.Status != apiv1.ResultSuccess || len(result.Artifacts) != count || len(rec.artifacts) != count {
				t.Fatalf("result %+v, records %d", result, len(rec.artifacts))
			}
			var index artifactset.Index
			if err := json.Unmarshal(rec.artifacts[count-1].data, &index); err != nil {
				t.Fatal(err)
			}
			if result.Artifacts[0].Digest != apiv1.Digest(rec.artifacts[count-1].data) {
				t.Fatal("index not slot zero")
			}
			if scenario == "valid" {
				if index.Entries[0].Name != "a.report" || index.Entries[0].Slot != 1 || index.Entries[0].Artifact != result.Artifacts[1] || string(rec.artifacts[0].data) != "[REDACTED] evidence" {
					t.Fatalf("bad normalized handoff: %+v, %q", index, rec.artifacts[0].data)
				}
			}
		})
	}
}

func TestExecutorReviewManifestArtifactSet(t *testing.T) {
	for _, reported := range []bool{false, true} {
		rec := &fakeRecorder{}
		adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
			manifest := artifactset.Manifest{SchemaVersion: artifactset.SchemaVersion, Entries: []artifactset.ManifestEntry{{Name: "review.report", Path: "report.txt", MediaType: "text/plain"}}}
			data, err := json.Marshal(manifest)
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(req.Workspace, "manifest.json"), data, 0o600); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(req.Workspace, "report.txt"), []byte("review evidence"), 0o600); err != nil {
				return err
			}
			verdict := apiv1.Verdict{Decision: apiv1.VerdictPass}
			if reported {
				verdict.Evidence = []apiv1.ArtifactPointer{{Path: "artifacts/forged", Digest: apiv1.Digest(nil)}}
			}
			return WriteCompletion(req.Workspace, req.CompletionPath, verdict)
		}}
		e, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewRegistryScrubber(), "")
		if err != nil {
			t.Fatal(err)
		}
		env := testEnvelope(t.TempDir())
		env.Inputs = map[string]interface{}{InputArtifactManifestFile: "manifest.json"}
		verdict, err := e.Review(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		if reported {
			if verdict.Decision != apiv1.VerdictFail || len(verdict.Evidence) != 0 || len(rec.artifacts) != 0 {
				t.Fatalf("untrusted review pointers accepted: %+v", verdict)
			}
		} else if verdict.Decision != apiv1.VerdictPass || len(verdict.Evidence) != 2 || verdict.Evidence[0].Digest != apiv1.Digest(rec.artifacts[1].data) {
			t.Fatalf("review did not publish index first: %+v", verdict)
		}
	}
}

func TestExecutorNamedSlotPublication(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "no-work", "self-reported", "no-manifest"} {
		t.Run(mode, func(t *testing.T) {
			rec := &fakeRecorder{}
			adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
				entries := []artifactset.ManifestEntry{}
				if mode != "missing" && mode != "no-work" {
					entries = append(entries, artifactset.ManifestEntry{Name: "report", Path: "payload", MediaType: "text/plain"})
				}
				data, err := json.Marshal(artifactset.Manifest{SchemaVersion: artifactset.SchemaVersion, Entries: entries})
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(req.Workspace, "manifest.json"), data, 0600); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(req.Workspace, "payload"), []byte("report"), 0600); err != nil {
					return err
				}
				result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}
				if mode == "no-work" {
					result.Status = apiv1.ResultNoWork
				}
				if mode == "self-reported" {
					result.Artifacts = []apiv1.ArtifactPointer{{Path: "artifacts/forged", Digest: apiv1.Digest(nil)}}
				}
				return WriteCompletion(req.Workspace, req.CompletionPath, result)
			}}
			e, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewRegistryScrubber(), "")
			if err != nil {
				t.Fatal(err)
			}
			env := testEnvelope(t.TempDir())
			env.Attempt = 2
			env.ArtifactPublication = &apiv1.ArtifactPublication{Stage: "produce", Visit: 11, Slots: []apiv1.ArtifactSlot{{Name: "report"}}}
			env.Inputs = map[string]any{InputArtifactManifestFile: "manifest.json"}
			if mode == "no-manifest" {
				env.Inputs = nil
			}
			result, err := e.Invoke(t.Context(), env)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "valid" {
				if len(result.Artifacts) != 2 {
					t.Fatalf("result=%+v", result)
				}
				var index artifactset.Index
				if err := json.Unmarshal(rec.artifacts[len(rec.artifacts)-1].data, &index); err != nil {
					t.Fatal(err)
				}
				if len(index.Bindings) != 1 || index.Bindings[0].Visit != 11 || index.Bindings[0].Attempt != 2 || index.Bindings[0].Artifact != result.Artifacts[1] {
					t.Fatalf("index=%+v", index)
				}
				return
			}
			code := artifactset.MissingSlotCode
			if mode == "self-reported" {
				code = artifactset.InvalidPublicationCode
			}
			if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != code || len(result.Artifacts) != 0 {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}
