package workerhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// THE WRITE PRIMITIVE'S OWN CONTAINMENT CHECK.
//
// This function is the only place in the fetch half that turns a DECLARED path
// into MkdirAll + WriteFile, and the path arrives on an envelope this process
// did not author: on a worker it comes from an upstream stage's surrendered
// ResultEnvelope (a bare json.Unmarshal in dispatcher.ReadSurrenderedResult,
// with no ArtifactPointer.Validate anywhere on that path), and since #3823 it
// also comes over the network into a stage pod. Every other declared-path call
// site in this repo applies the #120 containment primitive before it acts —
// ArtifactPointer.Resolve does — and a fetch that wrote first would place a new
// filesystem-write primitive AHEAD of that check, with the bytes already on
// disk by the time anything refused.
//
// Refused HARD, not skipped: an escaping path is not a cache miss to fill in
// later, it is an envelope that must not be acted on at all.
func TestMaterializeRefusesAPointerThatEscapesTheStagingRoot(t *testing.T) {
	t.Parallel()
	payload := []byte("#!/bin/sh\necho pwned\n")
	parent := t.TempDir()
	staging := filepath.Join(parent, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	escaped := filepath.Join(parent, "escaped.sh")

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "traversal", path: "../escaped.sh"},
		{name: "nested traversal", path: "artifacts/../../escaped.sh"},
		{name: "absolute", path: escaped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(escaped)
			store, err := blobstore.NewDir(t.TempDir())
			if err != nil {
				t.Fatalf("NewDir: %v", err)
			}
			ctx := context.Background()
			// The blob IS present under the digest named, so the refusal comes
			// from the PATH and not from a fetch that failed anyway.
			cp := pointerFor("upstream", payload)
			if err := store.Put(ctx, cp.Artifact.Digest, payload); err != nil {
				t.Fatalf("Put: %v", err)
			}
			cp.Artifact.Path = tc.path

			err = MaterializeContext(ctx, store, staging, []apiv1.ContextPointer{cp})
			// ASSERTED FIRST: whatever it returns, nothing may be written
			// outside the staging root.
			if data, readErr := os.ReadFile(escaped); readErr == nil {
				t.Fatalf("CONTAINMENT BREAK: wrote %d bytes outside the staging root at %s: %q (MaterializeContext returned %v)",
					len(data), escaped, data, err)
			}
			if err == nil {
				t.Fatal("MaterializeContext accepted a pointer whose path escapes the staging root")
			}
			if !errors.Is(err, apiv1.ErrPathEscape) {
				t.Fatalf("error = %v, want one wrapping apiv1.ErrPathEscape", err)
			}
			if !strings.Contains(err.Error(), "upstream") {
				t.Errorf("error %q does not name the pointer it refused", err.Error())
			}
		})
	}
}

// A pointer whose digest is not a real content address is refused too: it is
// the same structural check, and a digest the store can never hold would
// otherwise be fetched, missed, and left to fail later as a missing input.
func TestMaterializeRefusesAMalformedDigest(t *testing.T) {
	t.Parallel()
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	cp := pointerFor("upstream", []byte("data"))
	cp.Artifact.Digest = "sha256:not-hex"
	if err := MaterializeContext(context.Background(), store, t.TempDir(), []apiv1.ContextPointer{cp}); err == nil {
		t.Fatal("MaterializeContext accepted a pointer whose digest is not a content address")
	}
}
