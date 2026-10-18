package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestDirRoundTrip(t *testing.T) {
	t.Parallel()
	store, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	ctx := context.Background()
	data := []byte(`{"claimed":"issue-42"}`)
	d := digestOf(data)

	if has, err := store.Has(ctx, d); err != nil || has {
		t.Fatalf("Has before Put = %v, %v; want false, nil", has, err)
	}
	if _, err := store.Get(ctx, d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Put = %v; want ErrNotFound", err)
	}
	if err := store.Put(ctx, d, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, d)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("Get = %q; want %q", got, data)
	}
	if has, err := store.Has(ctx, d); err != nil || !has {
		t.Fatalf("Has after Put = %v, %v; want true, nil", has, err)
	}
}

func TestDirCorruptBlobIsNotFoundAndPutRecovers(t *testing.T) {
	t.Parallel()
	store, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	ctx := context.Background()
	data := []byte("correct bytes")
	d := digestOf(data)
	path, err := store.pathFor(d)
	if err != nil {
		t.Fatalf("pathFor: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir blob directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("corrupt bytes"), 0o644); err != nil {
		t.Fatalf("plant corrupt blob: %v", err)
	}

	if _, err := store.Get(ctx, d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get corrupt blob = %v; want ErrNotFound", err)
	}
	if err := store.Put(ctx, d, data); err != nil {
		t.Fatalf("Put correct bytes: %v", err)
	}
	got, err := store.Get(ctx, d)
	if err != nil {
		t.Fatalf("Get recovered blob: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("Get recovered blob = %q; want %q", got, data)
	}
}

// A repeated Put is the normal case, not the exceptional one: a retried
// activity re-records the same artifact, and a second worker may record a
// byte-identical result. It must succeed and must not rewrite.
func TestDirPutIsIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewDir(root)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	ctx := context.Background()
	data := []byte("same bytes")
	d := digestOf(data)
	if err := store.Put(ctx, d, data); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	rel, err := pathForDigest(d)
	if err != nil {
		t.Fatalf("pathForDigest: %v", err)
	}
	before, err := os.Stat(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := store.Put(ctx, d, data); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	after, err := os.Stat(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("second Put rewrote the blob (mtime %v -> %v)", before.ModTime(), after.ModTime())
	}
}

// The store is shared by a fleet with no lock. Concurrent Puts of one digest
// are the situation flock would otherwise be reached for, and they are safe
// here only because the write is staged and renamed — so this asserts the
// property rather than the implementation detail.
func TestDirConcurrentPutSameDigest(t *testing.T) {
	t.Parallel()
	store, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	ctx := context.Background()
	data := make([]byte, 1<<16)
	for i := range data {
		data[i] = byte(i)
	}
	d := digestOf(data)

	const writers = 16
	var wg sync.WaitGroup
	errs := make([]error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = store.Put(ctx, d, data)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	got, err := store.Get(ctx, d)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != len(data) {
		t.Fatalf("Get returned %d bytes; want %d — a reader saw a partial blob", len(got), len(data))
	}
}

// A digest arrives from a ContextPointer, which travels through Temporal
// history from an upstream stage. An unvalidated one is a path traversal with a
// remote origin, so rejection is a security property.
func TestDirRejectsMalformedDigest(t *testing.T) {
	t.Parallel()
	store, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	ctx := context.Background()
	for _, bad := range []string{
		"",
		"deadbeef",
		"sha256:../../../../etc/passwd",
		"sha256:" + "ZZ" + "00000000000000000000000000000000000000000000000000000000000000",
		"sha256:0011",
		"sha1:" + "00000000000000000000000000000000000000000000000000000000000000ab",
		"sha256:" + "AB00000000000000000000000000000000000000000000000000000000000000",
	} {
		if err := store.Put(ctx, bad, []byte("x")); err == nil {
			t.Errorf("Put(%q) succeeded; want rejection", bad)
		}
		if _, err := store.Get(ctx, bad); err == nil {
			t.Errorf("Get(%q) succeeded; want rejection", bad)
		}
		if _, err := store.Has(ctx, bad); err == nil {
			t.Errorf("Has(%q) succeeded; want rejection", bad)
		}
	}
}

func TestNewDirRejectsEmptyRoot(t *testing.T) {
	t.Parallel()
	if _, err := NewDir("  "); err == nil {
		t.Fatal("NewDir(\"  \") succeeded; want an error rather than a cwd-relative store")
	}
}

// The layout is the journal's own, so a store directory and a journal blob tree
// are structurally the same thing and one can seed the other by copying. That
// is a contract, not an accident, and it is asserted here so a refactor cannot
// quietly break the equivalence.
func TestDirLayoutMatchesJournal(t *testing.T) {
	t.Parallel()
	d := digestOf([]byte("x"))
	rel, err := pathForDigest(d)
	if err != nil {
		t.Fatalf("pathForDigest: %v", err)
	}
	hex := d[len("sha256:"):]
	want := filepath.Join("sha256", hex[:2], hex[2:])
	if rel != want {
		t.Fatalf("layout = %q; want %q", rel, want)
	}
}

// TestPutSucceedsWhenChmodUnsupported simulates the production Azure Files
// CIFS mount (nounix), where chmod returns EPERM (outage 2026-10-04).
func TestPutSucceedsWhenChmodUnsupported(t *testing.T) {
	old := chmodStaged
	chmodStaged = func(string, fs.FileMode) error {
		return &os.PathError{Op: "chmod", Path: "x", Err: syscall.EPERM}
	}
	t.Cleanup(func() { chmodStaged = old })

	dir, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("cifs blob")
	digest := digestOf(data)
	ctx := context.Background()
	if err := dir.Put(ctx, digest, data); err != nil {
		t.Fatalf("Put with EPERM chmod: %v", err)
	}
	got, err := dir.Get(ctx, digest)
	if err != nil || string(got) != string(data) {
		t.Fatalf("Get = %q, %v", got, err)
	}
}

// TestPutSucceedsWhenChmodAndLinkUnsupported simulates the CIFS blob share
// (nounix) rejecting both chmod and hard links with EPERM.
func TestPutSucceedsWhenChmodAndLinkUnsupported(t *testing.T) {
	oldChmod, oldLink := chmodStaged, linkStaged
	chmodStaged = func(string, fs.FileMode) error { return &os.PathError{Op: "chmod", Err: syscall.EPERM} }
	linkStaged = func(a, b string) error { return &os.LinkError{Op: "link", Old: a, New: b, Err: syscall.EPERM} }
	t.Cleanup(func() { chmodStaged, linkStaged = oldChmod, oldLink })

	dir, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	data := []byte("cifs blob without links")
	digest := digestOf(data)
	for i := 0; i < 2; i++ { // second Put is the idempotent path
		if err := dir.Put(ctx, digest, data); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	got, err := dir.Get(ctx, digest)
	if err != nil || string(got) != string(data) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	blobPath, err := dir.pathFor(digest)
	if err != nil {
		t.Fatal(err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(blobPath), ".put-*"))
	if len(leftovers) != 0 {
		t.Fatalf("staged files remain: %v", leftovers)
	}
}
