package workerhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
)

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func pointerFor(name string, data []byte) apiv1.ContextPointer {
	d := digestOf(data)
	hexPart := d[len("sha256:"):]
	return apiv1.ContextPointer{
		Name: name,
		Artifact: &apiv1.ArtifactPointer{
			Path:   "artifacts/sha256/" + hexPart[:2] + "/" + hexPart[2:],
			Digest: d,
			Size:   int64(len(data)),
		},
	}
}

// The whole point: a stage polled on a node that did not produce the artifact
// must still be able to read it.
func TestMaterializeFetchesBlobProducedElsewhere(t *testing.T) {
	t.Parallel()
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	ctx := context.Background()
	data := []byte(`{"claimed":"issue-42"}`)
	cp := pointerFor("claimed-item", data)
	// Node A recorded it: the store has the blob, this node's staging dir does not.
	if err := store.Put(ctx, cp.Artifact.Digest, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	staging := t.TempDir()
	if err := MaterializeContext(ctx, store, staging, []apiv1.ContextPointer{cp}); err != nil {
		t.Fatalf("MaterializeContext: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(staging, filepath.FromSlash(cp.Artifact.Path)))
	if err != nil {
		t.Fatalf("blob not materialized where Resolve will look: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("materialized %q; want %q", got, data)
	}
}

// A blob this worker produced must not be re-fetched or rewritten — the local
// directory is a cache, and a cache hit is the common case.
func TestMaterializeLeavesLocalBlobAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	data := []byte("local bytes")
	cp := pointerFor("local", data)

	staging := t.TempDir()
	dest := filepath.Join(staging, filepath.FromSlash(cp.Artifact.Path))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// An empty store: if Materialize tried to fetch, it would find nothing —
	// and must still not disturb what is already here.
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	if err := MaterializeContext(ctx, store, staging, []apiv1.ContextPointer{cp}); err != nil {
		t.Fatalf("MaterializeContext: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(data) {
		t.Fatalf("local blob disturbed: %q, %v", got, err)
	}
}

// A digest the store does not have is left absent, NOT raised here. The
// executor's own fail-closed integrity check must remain the thing that refuses
// the stage, with its own diagnostic and its own attempt classification;
// raising earlier would turn an integrity fault into an infrastructure one.
func TestMaterializeIsSilentOnMissingBlob(t *testing.T) {
	t.Parallel()
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	staging := t.TempDir()
	cp := pointerFor("absent", []byte("never stored"))
	if err := MaterializeContext(context.Background(), store, staging, []apiv1.ContextPointer{cp}); err != nil {
		t.Fatalf("MaterializeContext returned %v; a missing blob must be left to the executor's own check", err)
	}
	if _, err := os.Stat(filepath.Join(staging, filepath.FromSlash(cp.Artifact.Path))); !os.IsNotExist(err) {
		t.Fatalf("a blob appeared for a digest the store never held (stat err = %v)", err)
	}
}

type brokenStore struct{}

func (brokenStore) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("store unreachable")
}
func (brokenStore) Put(context.Context, string, []byte) error { return nil }
func (brokenStore) Has(context.Context, string) (bool, error) { return false, nil }
func (brokenStore) Describe() string                          { return "broken" }

// A broken store is not the same condition as an incomplete one and must not be
// silent — otherwise every stage on a fleet with a dead store fails later, as a
// confusing integrity fault, instead of now with the real reason.
func TestMaterializeSurfacesABrokenStore(t *testing.T) {
	t.Parallel()
	cp := pointerFor("x", []byte("data"))
	err := MaterializeContext(context.Background(), brokenStore{}, t.TempDir(), []apiv1.ContextPointer{cp})
	if err == nil {
		t.Fatal("MaterializeContext succeeded against a broken store; want an error naming it")
	}
}

// No store configured is the tier-1 shape: one runner, one directory, every
// blob local by construction. It must be a no-op, not an error.
func TestMaterializeNoStoreIsNoOp(t *testing.T) {
	t.Parallel()
	cp := pointerFor("x", []byte("data"))
	if err := MaterializeContext(context.Background(), nil, t.TempDir(), []apiv1.ContextPointer{cp}); err != nil {
		t.Fatalf("MaterializeContext with no store = %v; want nil", err)
	}
}
