package artifactset

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// countingReader records which payloads were actually read.
type countingReader struct {
	memoryReader
	reads map[string]int
}

func (r *countingReader) ReadArtifact(ctx context.Context, p apiv1.ArtifactPointer, limit int64) ([]byte, error) {
	r.reads[p.Digest]++
	return r.memoryReader.ReadArtifact(ctx, p, limit)
}

func publishNamedSet(t *testing.T, contract *apiv1.ArtifactPublication, files map[string]string) (*countingReader, []apiv1.ContextPointer) {
	t.Helper()
	root := t.TempDir()
	var entries []ManifestEntry
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, ManifestEntry{Name: name, Path: name, MediaType: "application/json"})
	}
	writeManifest(t, root, entries)
	identity := func(_ string, data []byte) ([]byte, error) { return data, nil }
	prepared, err := Prepare(t.Context(), root, "manifest.json", identity)
	if err != nil {
		t.Fatal(err)
	}
	if contract != nil {
		if err := prepared.Bind(contract, 3); err != nil {
			t.Fatal(err)
		}
	}
	reader := &countingReader{memoryReader: memoryReader{data: map[string][]byte{}}, reads: map[string]int{}}
	pointers, err := prepared.Publish(t.Context(), func(_ string, media string, data []byte) (apiv1.ArtifactPointer, error) {
		pointer := apiv1.ArtifactPointer{Path: "artifacts/" + apiv1.Digest(data)[7:], Digest: apiv1.Digest(data), MediaType: media, Size: int64(len(data)), Integrity: apiv1.IntegrityDerived}
		reader.data[pointer.Digest] = append([]byte(nil), data...)
		return pointer, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var inputs []apiv1.ContextPointer
	for i := range pointers {
		inputs = append(inputs, apiv1.ContextPointer{Name: fmt.Sprintf("triage.artifact[%d]", i), Artifact: &pointers[i]})
	}
	return reader, inputs
}

func TestResolveSlotBindsTheRecordedAttemptAndReadsOnlyThatPayload(t *testing.T) {
	contract := &apiv1.ArtifactPublication{Stage: "triage", Visit: 7, Slots: []apiv1.ArtifactSlot{{Name: "lenses", MediaType: "application/json"}, {Name: "notes"}}}
	reader, pointers := publishNamedSet(t, contract, map[string]string{"lenses": `{"schemaVersion":1,"branches":[]}`, "notes": `{"big":"unrelated"}`})
	payload, err := ResolveSlot(t.Context(), reader, pointers, "triage", "lenses", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Binding.Stage != "triage" || payload.Binding.Visit != 7 || payload.Binding.Attempt != 3 || payload.Binding.Slot != "lenses" || string(payload.Bytes) != `{"schemaVersion":1,"branches":[]}` {
		t.Fatalf("payload = %+v %q", payload.Binding, payload.Bytes)
	}
	if reader.reads[apiv1.Digest([]byte(`{"big":"unrelated"}`))] != 0 {
		t.Fatal("ResolveSlot read an unrelated payload")
	}
}

func TestResolveSlotRefusals(t *testing.T) {
	contract := &apiv1.ArtifactPublication{Stage: "triage", Visit: 7, Slots: []apiv1.ArtifactSlot{{Name: "lenses"}}}
	body := `{"schemaVersion":1,"branches":[]}`
	t.Run("size checked before the payload is read", func(t *testing.T) {
		reader, pointers := publishNamedSet(t, contract, map[string]string{"lenses": body})
		if _, err := ResolveSlot(t.Context(), reader, pointers, "triage", "lenses", int64(len(body)-1)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("oversize slot = %v", err)
		}
		if reader.reads[apiv1.Digest([]byte(body))] != 0 {
			t.Fatal("oversize payload was read before its size was refused")
		}
	})
	t.Run("missing slot", func(t *testing.T) {
		reader, pointers := publishNamedSet(t, contract, map[string]string{"lenses": body})
		var publication *PublicationError
		if _, err := ResolveSlot(t.Context(), reader, pointers, "triage", "absent", 1<<20); !errors.As(err, &publication) || publication.Code != MissingSlotCode {
			t.Fatalf("missing slot = %v", err)
		}
	})
	t.Run("unnamed legacy set", func(t *testing.T) {
		reader, pointers := publishNamedSet(t, nil, map[string]string{"lenses": body})
		if _, err := ResolveSlot(t.Context(), reader, pointers, "triage", "lenses", 1<<20); !errors.Is(err, ErrInvalid) {
			t.Fatalf("legacy set = %v", err)
		}
	})
	t.Run("tampered payload", func(t *testing.T) {
		reader, pointers := publishNamedSet(t, contract, map[string]string{"lenses": body})
		reader.data[apiv1.Digest([]byte(body))] = []byte(`{"schemaVersion":1,"branches":[{}]}`)
		if _, err := ResolveSlot(t.Context(), reader, pointers, "triage", "lenses", 1<<20); !errors.Is(err, ErrInvalid) {
			t.Fatalf("tampered payload = %v", err)
		}
	})
}
