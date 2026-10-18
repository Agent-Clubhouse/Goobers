package artifactset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestNamedSlotBindingsIgnoreOrderAndUnrelatedArtifacts(t *testing.T) {
	var original []SlotBinding
	for _, names := range [][]string{{"report", "evidence"}, {"evidence", "report"}, {"stdout", "evidence", "diagnostic", "report"}} {
		root := t.TempDir()
		var entries []ManifestEntry
		for _, name := range names {
			if err := os.WriteFile(filepath.Join(root, name), []byte(name+" secret"), 0600); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, ManifestEntry{Name: name, Path: name, MediaType: "text/plain"})
		}
		writeManifest(t, root, entries)
		prepared, err := Prepare(t.Context(), root, "manifest.json", testSanitize)
		if err != nil {
			t.Fatal(err)
		}
		contract := &apiv1.ArtifactPublication{Stage: "produce", Visit: 19, Slots: []apiv1.ArtifactSlot{{Name: "report"}, {Name: "evidence", MediaType: "text/plain", MaxSize: 100}}}
		if err := prepared.Bind(contract, 2); err != nil {
			t.Fatal(err)
		}
		// Mutating an invocation after admission cannot retarget a prepared set.
		contract.Slots[0].Name = "forged"
		blobs := &memoryReader{data: map[string][]byte{}}
		pointers, err := prepared.Publish(t.Context(), func(_ string, media string, data []byte) (apiv1.ArtifactPointer, error) {
			pointer := apiv1.ArtifactPointer{Path: "artifacts/" + apiv1.Digest(data)[7:], Digest: apiv1.Digest(data), MediaType: media, Size: int64(len(data)), Integrity: apiv1.IntegrityDerived}
			blobs.data[pointer.Digest] = append([]byte(nil), data...)
			return pointer, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var index Index
		if err := json.Unmarshal(blobs.data[pointers[0].Digest], &index); err != nil {
			t.Fatal(err)
		}
		if index.SchemaVersion != NamedSchemaVersion || len(index.Bindings) != 2 {
			t.Fatalf("index=%+v", index)
		}
		if original == nil {
			original = append([]SlotBinding(nil), index.Bindings...)
		} else if !reflect.DeepEqual(original, index.Bindings) {
			t.Fatalf("bindings changed: %+v vs %+v", original, index.Bindings)
		}
		var inputs []apiv1.ContextPointer
		for i := range pointers {
			inputs = append(inputs, apiv1.ContextPointer{Name: fmt.Sprintf("produce.artifact[%d]", i), Artifact: &pointers[i]})
		}
		if _, err := Resolve(context.Background(), blobs, inputs, "produce", "report", "evidence"); err != nil {
			t.Fatal(err)
		}
		index.Bindings[0].Artifact.Digest = apiv1.Digest([]byte("forged"))
		if err := validateBindings(index, "produce"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("forged binding accepted: %v", err)
		}
	}
}

func TestNamedSlotPublicationFailuresAreTyped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slots []apiv1.ArtifactSlot
		code  string
	}{
		{"missing", []apiv1.ArtifactSlot{{Name: "absent"}}, MissingSlotCode},
		{"duplicate", []apiv1.ArtifactSlot{{Name: "report"}, {Name: "report"}}, InvalidPublicationCode},
		{"media", []apiv1.ArtifactSlot{{Name: "report", MediaType: "application/json"}}, InvalidPublicationCode},
		{"size", []apiv1.ArtifactSlot{{Name: "report", MaxSize: 1}}, InvalidPublicationCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared := &Prepared{entries: []preparedEntry{{name: "report", mediaType: "text/plain", data: []byte("payload")}}}
			err := prepared.Bind(&apiv1.ArtifactPublication{Stage: "produce", Visit: 1, Slots: tc.slots}, 1)
			var publication *PublicationError
			if !errors.As(err, &publication) || publication.Code != tc.code {
				t.Fatalf("failure=%v", err)
			}
		})
	}
}

func TestNamedSlotAllowsCompleteDSLNameGrammar(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"_report", "-report"} {
		writeManifest(t, root, []ManifestEntry{{Name: name, Path: "payload", MediaType: "text/plain"}})
		prepared, err := Prepare(t.Context(), root, "manifest.json", testSanitize)
		if err != nil {
			t.Fatal(err)
		}
		if err := prepared.Bind(&apiv1.ArtifactPublication{Stage: "produce", Visit: 1, Slots: []apiv1.ArtifactSlot{{Name: name}}}, 1); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMissingPublication(t *testing.T) {
	tests := []struct {
		name     string
		contract *apiv1.ArtifactPublication
		slot     string
		message  string
	}{
		{"nil contract", nil, "", `missing_artifact_slot: slot ""`},
		{"empty slots", &apiv1.ArtifactPublication{}, "", `missing_artifact_slot: slot ""`},
		{"populated slots", &apiv1.ArtifactPublication{Slots: []apiv1.ArtifactSlot{{Name: "report"}, {Name: "evidence"}}}, "report", `missing_artifact_slot: slot "report"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := MissingPublication(tt.contract)
			var pe *PublicationError
			if !errors.As(err, &pe) {
				t.Fatalf("error = %T, want *PublicationError", err)
			}
			if pe.Code != MissingSlotCode || pe.Slot != tt.slot {
				t.Fatalf("code/slot = %q/%q", pe.Code, pe.Slot)
			}
			if err.Error() != tt.message {
				t.Fatalf("message = %q, want %q", err.Error(), tt.message)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatal("errors.Is(err, ErrInvalid) = false")
			}
		})
	}
}
