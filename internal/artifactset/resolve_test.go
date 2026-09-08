package artifactset

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type memoryReader struct {
	data  map[string][]byte
	err   error
	reads int
}

func (r *memoryReader) ReadArtifact(_ context.Context, p apiv1.ArtifactPointer, _ int64) ([]byte, error) {
	r.reads++
	if r.err != nil {
		return nil, r.err
	}
	return r.data[p.Digest], nil
}

func fixture(t *testing.T) (*memoryReader, []apiv1.ContextPointer, Index) {
	t.Helper()
	r := &memoryReader{data: map[string][]byte{}}
	data := []byte("sanitized evidence")
	p := apiv1.ArtifactPointer{Path: "artifacts/payload", Digest: apiv1.Digest(data), Size: int64(len(data)), MediaType: "text/plain"}
	r.data[p.Digest] = data
	index := Index{SchemaVersion: SchemaVersion, Entries: []Entry{{Name: "diagnosis.report", Slot: 1, Artifact: p}}}
	pointers := []apiv1.ContextPointer{{Name: "instrument.artifact[0]"}, {Name: "instrument.artifact[1]", Artifact: &p}}
	setIndex(t, r, pointers, index)
	return r, pointers, index
}

func setIndex(t *testing.T, r *memoryReader, pointers []apiv1.ContextPointer, index Index) {
	t.Helper()
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	setIndexBytes(r, pointers, data)
}

func setIndexBytes(r *memoryReader, pointers []apiv1.ContextPointer, data []byte) {
	p := apiv1.ArtifactPointer{Path: "artifacts/index", Digest: apiv1.Digest(data), Size: int64(len(data)), MediaType: "application/json"}
	r.data[p.Digest] = data
	pointers[0].Artifact = &p
}

func TestResolveVerifiedSet(t *testing.T) {
	r, pointers, _ := fixture(t)
	got, err := Resolve(context.Background(), r, pointers, "instrument", "diagnosis.report")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got["diagnosis.report"].Bytes) != "sanitized evidence" || got["diagnosis.report"].Artifact != *pointers[1].Artifact {
		t.Fatalf("unexpected lookup: %+v", got)
	}
}

func TestResolveRejectsInvalidSets(t *testing.T) {
	tests := map[string]func(*memoryReader, []apiv1.ContextPointer, *Index){
		"version":        func(_ *memoryReader, _ []apiv1.ContextPointer, i *Index) { i.SchemaVersion = "future" },
		"null entries":   func(_ *memoryReader, _ []apiv1.ContextPointer, i *Index) { i.Entries = nil },
		"duplicate name": func(_ *memoryReader, _ []apiv1.ContextPointer, i *Index) { i.Entries = append(i.Entries, i.Entries[0]) },
		"slot zero":      func(_ *memoryReader, _ []apiv1.ContextPointer, i *Index) { i.Entries[0].Slot = 0 },
		"slot gap":       func(_ *memoryReader, _ []apiv1.ContextPointer, i *Index) { i.Entries[0].Slot = 2 },
		"unsafe name":    func(_ *memoryReader, _ []apiv1.ContextPointer, i *Index) { i.Entries[0].Name = "../secret" },
		"pointer mismatch": func(_ *memoryReader, _ []apiv1.ContextPointer, i *Index) {
			i.Entries[0].Artifact.MediaType = "application/json"
		},
		"cross run": func(_ *memoryReader, p []apiv1.ContextPointer, _ *Index) { p[1].RunID = "other" },
		"external": func(_ *memoryReader, p []apiv1.ContextPointer, _ *Index) {
			p[1].External = &apiv1.ExternalRef{Kind: "url", URI: "https://example.com"}
		},
		"branch ambiguity": func(_ *memoryReader, p []apiv1.ContextPointer, _ *Index) { p[1].Branch = 1 },
		"missing pointer":  func(_ *memoryReader, p []apiv1.ContextPointer, _ *Index) { p[1].Name = "other.artifact[1]" },
		"digest mismatch": func(r *memoryReader, p []apiv1.ContextPointer, _ *Index) {
			r.data[p[1].Artifact.Digest] = []byte("tampered")
		},
		"oversized": func(_ *memoryReader, p []apiv1.ContextPointer, i *Index) {
			p[1].Artifact.Size = MaxPayloadBytes + 1
			i.Entries[0].Artifact = *p[1].Artifact
		},
		"path escape": func(_ *memoryReader, p []apiv1.ContextPointer, i *Index) {
			p[1].Artifact.Path = "../secret"
			i.Entries[0].Artifact = *p[1].Artifact
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			r, pointers, index := fixture(t)
			mutate(r, pointers, &index)
			setIndex(t, r, pointers, index)
			got, err := Resolve(context.Background(), r, pointers, "instrument")
			if !errors.Is(err, ErrInvalid) || got != nil {
				t.Fatalf("got %v, %v; want invalid, no partial lookup", got, err)
			}
		})
	}
}

func TestResolveRejectsMalformedJSON(t *testing.T) {
	for _, data := range []string{
		`{"schemaVersion":"old","schemaVersion":"` + SchemaVersion + `","entries":[]}`,
		`{"schemaVersion":"` + SchemaVersion + `","entries":[],"extra":true}`,
		`{"schemaVersion":"` + SchemaVersion + `","entries":[]} {}`,
		`null`, `[]`, `{"entries":`, strings.Repeat("[", 18) + strings.Repeat("]", 18),
	} {
		r, pointers, _ := fixture(t)
		setIndexBytes(r, pointers, []byte(data))
		if got, err := Resolve(context.Background(), r, pointers, "instrument"); !errors.Is(err, ErrInvalid) || got != nil {
			t.Fatalf("accepted %q: %v", data, err)
		}
	}
}

func TestResolveEmptyAndRequiredNames(t *testing.T) {
	r, pointers, _ := fixture(t)
	setIndex(t, r, pointers, Index{SchemaVersion: SchemaVersion, Entries: []Entry{}})
	if got, err := Resolve(context.Background(), r, pointers, "instrument"); err != nil || len(got) != 0 {
		t.Fatalf("empty set: %v, %v", got, err)
	}
	if got, err := Resolve(context.Background(), r, pointers, "instrument", "missing"); !errors.Is(err, ErrInvalid) || got != nil {
		t.Fatalf("required name: %v, %v", got, err)
	}
}

func TestResolvePreservesReaderError(t *testing.T) {
	r, pointers, _ := fixture(t)
	r.err = errors.New("temporary journal failure")
	if got, err := Resolve(context.Background(), r, pointers, "instrument"); !errors.Is(err, r.err) || errors.Is(err, ErrInvalid) || got != nil {
		t.Fatalf("reader error: %v, %v", got, err)
	}
}

func TestResolveDuplicateContextRefused(t *testing.T) {
	r, pointers, _ := fixture(t)
	pointers = append(pointers, pointers[1])
	if got, err := Resolve(context.Background(), r, pointers, "instrument"); !errors.Is(err, ErrInvalid) || got != nil {
		t.Fatalf("duplicate context: %v, %v", got, err)
	}
}
