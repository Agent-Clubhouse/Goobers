package workerhost

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

func TestWorkerNamedSlotIndexSurvivesJournalAdoption(t *testing.T) {
	workspace := t.TempDir()
	manifest := artifactset.Manifest{SchemaVersion: artifactset.SchemaVersion, Entries: []artifactset.ManifestEntry{{Name: "report", Path: "report", MediaType: "text/plain"}}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "manifest"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "report"), []byte("evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := artifactset.Prepare(t.Context(), workspace, "manifest", artifactset.NewSanitizer(journal.NewRegistryScrubber()))
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Bind(&apiv1.ArtifactPublication{Stage: "produce", Visit: 21, Slots: []apiv1.ArtifactSlot{{Name: "report"}}}, 2); err != nil {
		t.Fatal(err)
	}
	store := NewStagingArtifacts(t.TempDir(), journal.NewRegistryScrubber(), nil)
	pointers, err := prepared.Publish(t.Context(), func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
		ref, err := store.RecordPreparedArtifact(t.Context(), name, media, data)
		return apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, Integrity: ref.Integrity, MediaType: media}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: "adopt", Workflow: "slots", WorkflowVersion: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := run.Dir()
	for i, pointer := range pointers {
		bytes, err := pointer.Resolve(store.Dir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := run.RecordStageArtifact("produce", 2, "", manifestName(i), bytes); err != nil {
			t.Fatal(err)
		}
	}
	refs := make([]journal.Ref, len(pointers))
	for i, p := range pointers {
		refs[i] = journal.Ref{Path: p.Path, Digest: p.Digest, Size: p.Size, Integrity: p.Integrity}
	}
	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "produce", Attempt: 2, Status: "success", Artifacts: refs}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	bytes, err := pointers[0].Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	var index artifactset.Index
	if err := json.Unmarshal(bytes, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Bindings) != 1 || index.Bindings[0].Artifact != pointers[1] || index.Bindings[0].Visit != 21 || index.Bindings[0].Attempt != 2 || index.Bindings[0].Artifact.Integrity != apiv1.IntegrityDerived {
		t.Fatalf("index=%+v", index)
	}
}
func manifestName(i int) string {
	if i == 0 {
		return "artifact-set.json"
	}
	return "report"
}

type refusingPublicationStore struct{}

func (refusingPublicationStore) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("unavailable")
}
func (refusingPublicationStore) Put(context.Context, string, []byte) error {
	return errors.New("publication unavailable")
}
func (refusingPublicationStore) Has(context.Context, string) (bool, error) { return false, nil }
func (refusingPublicationStore) Describe() string                          { return "refusing publication" }

func TestWorkerNamedPublicationRequiresDurableFleetWrite(t *testing.T) {
	s := NewStagingArtifacts(t.TempDir(), fixedScrubber{}, refusingPublicationStore{})
	if _, err := s.RecordPreparedArtifact(t.Context(), "report", "text/plain", []byte("prepared")); err == nil {
		t.Fatal("publication succeeded without durable fleet bytes")
	}
	if len(s.StoreErrors()) != 0 {
		t.Fatal("durable failure hidden as a diagnostic")
	}
	if _, err := s.RecordArtifact("diagnostic", []byte("diagnostic")); err != nil {
		t.Fatal("legacy diagnostics changed", err)
	}
	if len(s.StoreErrors()) != 1 {
		t.Fatal("legacy diagnostic failure was not recorded")
	}
	s.Store = nil
	ref, err := s.RecordPreparedArtifact(t.Context(), "report", "text/plain", []byte("already sanitized s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Digest != apiv1.Digest([]byte("already sanitized s3cret")) {
		t.Fatal("prepared bytes were rewritten")
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), filepath.FromSlash(ref.Path)), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	repaired, err := s.RecordPreparedArtifact(t.Context(), "report", "text/plain", []byte("already sanitized s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (apiv1.ArtifactPointer{Path: repaired.Path, Digest: repaired.Digest}).Resolve(s.Dir()); err != nil {
		t.Fatal("cached corrupt bytes not repaired", err)
	}
}
