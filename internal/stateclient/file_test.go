package stateclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testFileStore(t *testing.T, dir string) *File {
	t.Helper()
	store, err := NewFile(FileConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestValidKeyIsAClosedNamespace is the containment guarantee the whole plane
// rests on: a state bearer can only ever address these five shapes, so it can
// never be turned into a read or a write of claims.json, the instance config,
// or anything else that shares the scheduler directory.
func TestValidKeyIsAClosedNamespace(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	for _, key := range []string{
		KeyBlockedRecords,
		KeyPostMergeReconcileLedger,
		KeySiblingContextCache,
		ScanCursorKey(digest),
		// Goobers#3898: the backlog re-sweep generation, the last scheduler
		// file the claiming path held open directly.
		ResweepStateKey(digest),
	} {
		if !ValidKey(key) {
			t.Fatalf("ValidKey(%q) = false, want the closed namespace to admit it", key)
		}
	}
	for _, key := range []string{
		"",
		"claims.json",
		"config.yaml",
		"instance.yaml",
		"../config.yaml",
		"blocked.json/../claims.json",
		"Blocked.json",
		"blocked.json.tmp",
		".blocked.json",
		"backlog-scan-.json",
		"backlog-scan-" + strings.ToUpper(digest) + ".json",
		"backlog-scan-" + digest[:63] + ".json",
		"backlog-scan-" + digest + "x.json",
		"backlog-scan-" + digest + ".json.tmp",
		"backlog-resweep-.json",
		"backlog-resweep-" + strings.ToUpper(digest) + ".json",
		"backlog-resweep-" + digest[:63] + ".json",
		"backlog-resweep-" + digest + "x.json",
		"backlog-resweep-" + digest + ".json.tmp",
		"backlog-resweep-" + digest + ".json/../claims.json",
		"sub/blocked.json",
	} {
		if ValidKey(key) {
			t.Fatalf("ValidKey(%q) = true, want it refused", key)
		}
	}
}

// TestSelectFailsClosed pins the refusal that keeps a stage pod from silently
// writing a scheduler-state file nothing on the other side will ever read.
func TestSelectFailsClosed(t *testing.T) {
	fileCalls := 0
	file := func() (Store, error) {
		fileCalls++
		return testFileStore(t, t.TempDir()), nil
	}
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}

	store, err := Select(env(nil), file)
	if err != nil || store == nil {
		t.Fatalf("no endpoint: store = %v, err = %v, want the file backend", store, err)
	}
	if fileCalls != 1 {
		t.Fatalf("file backend built %d times, want 1", fileCalls)
	}
	if Selected(env(nil)) {
		t.Fatal("Selected reported the plane with no endpoint configured")
	}

	if _, err := Select(env(map[string]string{EnvEndpoint: "http://daemon"}), file); !errors.Is(err, ErrEndpointWithoutToken) {
		t.Fatalf("endpoint without a bearer: err = %v, want ErrEndpointWithoutToken", err)
	}
	if _, err := Select(env(map[string]string{EnvEndpoint: "http://daemon", EnvToken: "t"}), file); !errors.Is(err, ErrEndpointWithoutGaggle) {
		t.Fatalf("endpoint without a gaggle: err = %v, want ErrEndpointWithoutGaggle", err)
	}
	if fileCalls != 1 {
		t.Fatal("a misconfigured plane fell through to the file backend; it must fail closed")
	}

	store, err = Select(env(map[string]string{EnvEndpoint: "http://daemon", EnvToken: "t", EnvGaggle: "goobers"}), file)
	if err != nil {
		t.Fatal(err)
	}
	plane, ok := store.(*HTTP)
	if !ok {
		t.Fatalf("store = %T, want the plane backend", store)
	}
	if plane.Gaggle() != "goobers" {
		t.Fatalf("gaggle = %q", plane.Gaggle())
	}
	if !Selected(env(map[string]string{EnvEndpoint: "http://daemon"})) {
		t.Fatal("Selected did not report a configured endpoint")
	}
}

// TestFileStoreAbsentKeyReadsAsZeroValue pins the first-run state every one of
// these files starts in: absent is not an error, and its empty ETag doubles as
// the "must still be absent" precondition.
func TestFileStoreAbsentKeyReadsAsZeroValue(t *testing.T) {
	store := testFileStore(t, t.TempDir())
	value, err := store.Get(t.Context(), KeyBlockedRecords)
	if err != nil {
		t.Fatal(err)
	}
	if value.Exists() || value.Data != nil || value.ETag != "" {
		t.Fatalf("value = %+v, want the zero value", value)
	}
}

func TestFileStoreCompareAndSwap(t *testing.T) {
	dir := t.TempDir()
	store := testFileStore(t, dir)
	ctx := t.Context()

	// Create-if-absent.
	created, err := store.Put(ctx, KeyBlockedRecords, []byte(`{"a":1}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if created.ETag != ETagFor([]byte(`{"a":1}`)) {
		t.Fatalf("etag = %q, want the content digest", created.ETag)
	}
	if _, err := os.Stat(filepath.Join(dir, KeyBlockedRecords)); err != nil {
		t.Fatalf("the value did not land in the scheduler directory: %v", err)
	}

	// Creating again must lose: the key is no longer absent.
	if _, err := store.Put(ctx, KeyBlockedRecords, []byte(`{"b":2}`), ""); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("second create err = %v, want ErrPreconditionFailed", err)
	}

	// A stale ETag must lose.
	if _, err := store.Put(ctx, KeyBlockedRecords, []byte(`{"c":3}`), ETagFor([]byte("stale"))); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale-etag write err = %v, want ErrPreconditionFailed", err)
	}

	// The current ETag must win, and yield a new one.
	replaced, err := store.Put(ctx, KeyBlockedRecords, []byte(`{"d":4}`), created.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.ETag == created.ETag {
		t.Fatal("the replacement kept the old etag; a session chaining writes would then reuse a stale precondition")
	}
	value, err := store.Get(ctx, KeyBlockedRecords)
	if err != nil {
		t.Fatal(err)
	}
	if string(value.Data) != `{"d":4}` || value.ETag != replaced.ETag {
		t.Fatalf("value = %+v, want the replacement", value)
	}
}

func TestFileStoreRefusesKeysOutsideTheNamespace(t *testing.T) {
	dir := t.TempDir()
	store := testFileStore(t, dir)
	ctx := t.Context()

	for _, key := range []string{"claims.json", "../config.yaml", "blocked.json/../claims.json"} {
		if _, err := store.Get(ctx, key); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Get(%q) err = %v, want ErrInvalidKey", key, err)
		}
		if _, err := store.Put(ctx, key, []byte("{}"), ""); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Put(%q) err = %v, want ErrInvalidKey", key, err)
		}
		if err := store.Update(ctx, key, "op", func(Value) ([]byte, bool, error) {
			t.Fatalf("Update(%q) ran its body for a refused key", key)
			return nil, false, nil
		}); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Update(%q) err = %v, want ErrInvalidKey", key, err)
		}
	}
	// Nothing outside the namespace may have been created along the way.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused keys created %v", entries)
	}
}

// TestFileStoreUpdateRunsInsideTheLock is the file backend's atomicity claim:
// the read and the write are one lock acquisition, so a concurrent writer
// cannot interleave between them and no update is lost.
func TestFileStoreUpdateRunsInsideTheLock(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	held := 0
	maxHeld := 0
	lock := func(_, _ string, fn func() error) error {
		mu.Lock()
		held++
		if held > maxHeld {
			maxHeld = held
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			held--
			mu.Unlock()
		}()
		return fn()
	}
	store, err := NewFile(FileConfig{Dir: dir, Lock: func(key, operation string, fn func() error) error {
		// One process-wide serialization, standing in for the flock the real
		// caller injects.
		return lock(key, operation, func() error { return fn() })
	}})
	if err != nil {
		t.Fatal(err)
	}

	var readsInsideLock int
	if err := store.Update(t.Context(), KeyBlockedRecords, "op", func(value Value) ([]byte, bool, error) {
		mu.Lock()
		readsInsideLock = held
		mu.Unlock()
		if value.Exists() {
			t.Fatal("first update saw an existing value")
		}
		return []byte(`{"n":1}`), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if readsInsideLock != 1 {
		t.Fatalf("update body ran with %d locks held, want it inside the key's lock", readsInsideLock)
	}
	value, err := store.Get(t.Context(), KeyBlockedRecords)
	if err != nil {
		t.Fatal(err)
	}
	if string(value.Data) != `{"n":1}` {
		t.Fatalf("value = %s", value.Data)
	}
}

// TestFileStoreUpdateSkipsTheWriteWhenNothingChanged pins the common reconcile
// outcome: no drift means no write, so an unchanged key keeps its ETag and
// concurrent readers are never spuriously invalidated.
func TestFileStoreUpdateSkipsTheWriteWhenNothingChanged(t *testing.T) {
	dir := t.TempDir()
	store := testFileStore(t, dir)
	ctx := t.Context()
	before, err := store.Put(ctx, KeyBlockedRecords, []byte(`{"a":1}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(ctx, KeyBlockedRecords, "op", func(Value) ([]byte, bool, error) {
		return []byte(`{"ignored":true}`), false, nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := store.Get(ctx, KeyBlockedRecords)
	if err != nil {
		t.Fatal(err)
	}
	if after.ETag != before.ETag || string(after.Data) != `{"a":1}` {
		t.Fatalf("value = %+v, want it untouched", after)
	}
}

// TestFileStoreHeldVariantTakesNoLock covers the seam the claim transaction
// uses: a caller already standing inside claims.lock must not take it again,
// or it would wait on itself until the timeout.
func TestFileStoreHeldVariantTakesNoLock(t *testing.T) {
	store := testFileStore(t, t.TempDir())
	if err := store.Update(t.Context(), KeyBlockedRecords, "op", func(Value) ([]byte, bool, error) {
		return []byte("{}"), true, nil
	}); err != nil {
		t.Fatalf("held store update: %v", err)
	}
}

// TestFileStoreSectionHoldsTheLockAcrossTheWholeBody is the post-merge
// reconcile's requirement: it reads once and then persists progress repeatedly
// while polling providers, and no other writer may interleave.
func TestFileStoreSectionHoldsTheLockAcrossTheWholeBody(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	holds := 0
	store, err := NewFile(FileConfig{Dir: dir, Lock: func(_, _ string, fn func() error) error {
		mu.Lock()
		holds++
		mu.Unlock()
		return fn()
	}})
	if err != nil {
		t.Fatal(err)
	}
	held, err := NewFile(FileConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := store.Section(ctx, KeyPostMergeReconcileLedger, "op", func() error {
		first, err := held.Put(ctx, KeyPostMergeReconcileLedger, []byte(`{"step":1}`), "")
		if err != nil {
			return err
		}
		_, err = held.Put(ctx, KeyPostMergeReconcileLedger, []byte(`{"step":2}`), first.ETag)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if holds != 1 {
		t.Fatalf("lock taken %d times, want exactly one section for the whole body", holds)
	}
	value, err := held.Get(ctx, KeyPostMergeReconcileLedger)
	if err != nil {
		t.Fatal(err)
	}
	if string(value.Data) != `{"step":2}` {
		t.Fatalf("value = %s, want the section's last write", value.Data)
	}
}

func TestFileStoreRefusesOversizedValues(t *testing.T) {
	store := testFileStore(t, t.TempDir())
	if _, err := store.Put(t.Context(), KeyBlockedRecords, make([]byte, MaxValueBytes+1), ""); err == nil {
		t.Fatal("an oversized value was accepted")
	}
}

func TestETagForIsContentAddressed(t *testing.T) {
	first, second := ETagFor([]byte("x")), ETagFor(append([]byte{}, 'x'))
	if first != second {
		t.Fatal("identical bytes hashed to different etags")
	}
	if ETagFor([]byte("x")) == ETagFor([]byte("y")) {
		t.Fatal("different bytes hashed to the same etag")
	}
	// Two writers producing byte-identical output must not refuse each other.
	if ETagFor(nil) == "" {
		t.Fatal("the empty value has no etag, so it could never be compare-and-swapped")
	}
}

// TestFilePriorityTriggerIsUnavailable keeps the fallback honest: there is no
// daemon behind a file, and minting locally would bypass scheduler admission.
func TestFilePriorityTriggerIsUnavailable(t *testing.T) {
	store := testFileStore(t, t.TempDir())
	if _, err := store.PriorityTrigger(context.Background(), "merge-review", "run-1"); !errors.Is(err, ErrPriorityTriggerUnavailable) {
		t.Fatalf("err = %v, want ErrPriorityTriggerUnavailable", err)
	}
}
