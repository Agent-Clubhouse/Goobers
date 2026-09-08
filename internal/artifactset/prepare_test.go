package artifactset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func writeManifest(t *testing.T, root string, entries []ManifestEntry) {
	t.Helper()
	data, err := json.Marshal(Manifest{SchemaVersion: SchemaVersion, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func testSanitize(media string, data []byte) ([]byte, error) {
	if media != "text/plain" {
		return nil, errors.New("unsupported media")
	}
	return bytes.ReplaceAll(data, []byte("secret"), []byte("[redacted]")), nil
}

func TestPreparePublishResolveRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("secret evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, root, []ManifestEntry{{Name: "z.last", Path: "payload", MediaType: "text/plain"}, {Name: "a.first", Path: "payload", MediaType: "text/plain"}})
	prepared, err := Prepare(context.Background(), root, "manifest.json", testSanitize)
	if err != nil {
		t.Fatal(err)
	}
	r := &memoryReader{data: make(map[string][]byte)}
	var names []string
	pointers, err := prepared.Publish(context.Background(), func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
		names = append(names, name)
		p := apiv1.ArtifactPointer{Path: "artifacts/" + name, Digest: apiv1.Digest(data), MediaType: media, Size: int64(len(data))}
		r.data[p.Digest] = append([]byte(nil), data...)
		return p, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 || names[0] != "a.first" || names[1] != "z.last" || names[2] != "artifact-set.json" {
		t.Fatalf("record order: %v", names)
	}
	contextPointers := make([]apiv1.ContextPointer, len(pointers))
	for i := range pointers {
		contextPointers[i] = apiv1.ContextPointer{Name: []string{"instrument.artifact[0]", "instrument.artifact[1]", "instrument.artifact[2]"}[i], Artifact: &pointers[i]}
	}
	got, err := Resolve(context.Background(), r, contextPointers, "instrument", "a.first", "z.last")
	if err != nil {
		t.Fatal(err)
	}
	if string(got["a.first"].Bytes) != "[redacted] evidence" {
		t.Fatalf("unsanitized payload: %q", got["a.first"].Bytes)
	}
}

func TestPrepareRejectsEntireInvalidSet(t *testing.T) {
	for name, entry := range map[string]ManifestEntry{
		"missing":     {Name: "b", Path: "missing", MediaType: "text/plain"},
		"directory":   {Name: "b", Path: "directory", MediaType: "text/plain"},
		"duplicate":   {Name: "a", Path: "payload", MediaType: "text/plain"},
		"escape":      {Name: "b", Path: "../payload", MediaType: "text/plain"},
		"unsupported": {Name: "b", Path: "payload", MediaType: "unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "payload"), []byte("evidence"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeManifest(t, root, []ManifestEntry{{Name: "a", Path: "payload", MediaType: "text/plain"}, entry})
			if got, err := Prepare(context.Background(), root, "manifest.json", testSanitize); got != nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}

func TestPublishNeverReturnsPartialResult(t *testing.T) {
	p := &Prepared{entries: []preparedEntry{{name: "a", mediaType: "text/plain", data: []byte("one")}, {name: "b", mediaType: "text/plain", data: []byte("two")}}}
	failure := errors.New("disk failure")
	for _, failAt := range []int{1, 2, 3} {
		calls := 0
		got, err := p.Publish(context.Background(), func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
			calls++
			if calls == failAt {
				return apiv1.ArtifactPointer{}, failure
			}
			return apiv1.ArtifactPointer{Path: "artifacts/" + name, Digest: apiv1.Digest(data), MediaType: media, Size: int64(len(data))}, nil
		})
		if got != nil || !errors.Is(err, failure) {
			t.Fatalf("failure at %d: %v, %v", failAt, got, err)
		}
	}
}

func TestPrepareBoundsAndRequiredPolicy(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, []ManifestEntry{})
	if p, err := Prepare(context.Background(), root, "manifest.json", nil); p != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing policy: %v, %v", p, err)
	}
	entries := make([]ManifestEntry, MaxEntries+1)
	writeManifest(t, root, entries)
	if p, err := Prepare(context.Background(), root, "manifest.json", testSanitize); p != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("entry count: %v, %v", p, err)
	}
	file, err := os.Create(filepath.Join(root, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxPayloadBytes + 1); err != nil {
		t.Fatal(errors.Join(err, file.Close()))
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, root, []ManifestEntry{{Name: "large", Path: "large", MediaType: "text/plain"}})
	called := false
	p, err := Prepare(context.Background(), root, "manifest.json", func(_ string, data []byte) ([]byte, error) { called = true; return data, nil })
	if p != nil || !errors.Is(err, ErrInvalid) || called {
		t.Fatalf("oversized file reached sanitizer: %v, %v, %v", p, err, called)
	}
}

func TestPublishRejectsRecorderMutation(t *testing.T) {
	p := &Prepared{entries: []preparedEntry{{name: "a", mediaType: "text/plain", data: []byte("safe")}}}
	got, err := p.Publish(context.Background(), func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
		return apiv1.ArtifactPointer{Path: "artifacts/" + name, Digest: apiv1.Digest([]byte("changed")), MediaType: media, Size: int64(len(data))}, nil
	})
	if got != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("recorder changed payload: %v, %v", got, err)
	}
}
