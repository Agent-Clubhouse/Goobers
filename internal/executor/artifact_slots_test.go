package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/journal"
)

func TestShellPublishesNamedSlotsAlongsideDiagnostics(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "published", true: "missing"}[missing], func(t *testing.T) {
			e, rec := newTestExecutor(t, nil)
			root := t.TempDir()
			entries := []artifactset.ManifestEntry{}
			if !missing {
				entries = append(entries, artifactset.ManifestEntry{Name: "report", Path: "report.txt", MediaType: "text/plain"})
			}
			data, err := json.Marshal(artifactset.Manifest{SchemaVersion: artifactset.SchemaVersion, Entries: entries})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "manifest.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "report.txt"), []byte("evidence"), 0600); err != nil {
				t.Fatal(err)
			}
			env := apiv1.InvocationEnvelope{TaskID: "run:produce", Workspace: root, Attempt: 3, Inputs: map[string]any{"artifactManifestFile": "manifest.json"}, ArtifactPublication: &apiv1.ArtifactPublication{Stage: "produce", Visit: 7, Slots: []apiv1.ArtifactSlot{{Name: "report"}}}}
			result, err := e.Run(t.Context(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "printf diagnostic; printf diagnostic >&2"}})
			if err != nil {
				t.Fatal(err)
			}
			if missing {
				if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != artifactset.MissingSlotCode {
					t.Fatalf("result=%+v", result)
				}
				if _, ok := rec.recorded[env.TaskID+"/artifact-set.json"]; ok {
					t.Fatal("partial named index published")
				}
				return
			}
			if result.Status != apiv1.ResultSuccess || len(result.Artifacts) != 4 {
				t.Fatalf("result=%+v", result)
			}
			var index artifactset.Index
			if err := json.Unmarshal(rec.recorded[env.TaskID+"/artifact-set.json"], &index); err != nil {
				t.Fatal(err)
			}
			if len(index.Bindings) != 1 || index.Bindings[0].Stage != "produce" || index.Bindings[0].Visit != 7 || index.Bindings[0].Attempt != 3 || index.Bindings[0].Artifact != result.Artifacts[1] {
				t.Fatalf("index=%+v", index)
			}
			if result.Artifacts[0].Digest != apiv1.Digest(rec.recorded[env.TaskID+"/artifact-set.json"]) {
				t.Fatal("index lost positional alias")
			}
		})
	}
}

type durableSlotRecorder struct {
	*fakeRecorder
	calls int
	fail  bool
}

func (r *durableSlotRecorder) RecordPreparedArtifact(_ context.Context, name, media string, data []byte) (journal.Ref, error) {
	r.calls++
	if r.fail {
		return journal.Ref{}, errors.New("durable write failed")
	}
	return r.RecordArtifact(name, data)
}
func TestShellNamedPublicationUsesDurableRecorder(t *testing.T) {
	e, rec := newPortableTestExecutor(t, nil)
	durable := &durableSlotRecorder{fakeRecorder: rec}
	e.Journal = durable
	if _, err := e.recordPreparedSlot(t.Context(), "report", "text/plain", []byte("bytes")); err != nil {
		t.Fatal(err)
	}
	durable.fail = true
	if _, err := e.recordPreparedSlot(t.Context(), "report", "text/plain", []byte("bytes")); err == nil {
		t.Fatal("durability failure ignored")
	}
	if durable.calls != 2 {
		t.Fatalf("prepared recorder calls=%d", durable.calls)
	}
}
