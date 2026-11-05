// Package receipts is the cancel-receipt backend conformance suite. Every
// cancelreceipt.Receipts implementation runs it, so cloud receipts keep the
// semantics of the local reference store.
package receipts

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/cancelreceipt"
)

// Backend opens views of one durable store. Every Open after the first must
// observe what earlier views committed, as a restarted process would.
type Backend struct {
	Open func(t *testing.T) cancelreceipt.Receipts
}

// Run executes the suite. newBackend returns an empty store per subtest.
func Run(t *testing.T, newBackend func(t *testing.T) Backend) {
	cases := map[string]func(*testing.T, Backend){
		"fresh-then-existing":      freshThenExisting,
		"mismatch-conflicts":       mismatchConflicts,
		"finish-once":              finishOnce,
		"finish-unknown":           finishUnknown,
		"invalid-requests":         invalidRequests,
		"durable-across-reopen":    durableAcrossReopen,
		"concurrent-begin-one-win": concurrentBegin,
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) { run(t, newBackend(t)) })
	}
}

func begin(t *testing.T, store cancelreceipt.Receipts, key, actor, payload string) (cancelreceipt.Receipt, bool) {
	t.Helper()
	receipt, fresh, err := store.Begin(t.Context(), key, actor, []byte(payload))
	if err != nil {
		t.Fatalf("Begin(%q): %v", key, err)
	}
	return receipt, fresh
}

func freshThenExisting(t *testing.T, backend Backend) {
	store := backend.Open(t)
	if _, fresh := begin(t, store, "k", "alice", "cancel run-1"); !fresh {
		t.Fatal("first Begin was not fresh")
	}
	receipt, fresh := begin(t, store, "k", "alice", "cancel run-1")
	if fresh || receipt.Complete || receipt.Result != nil {
		t.Fatalf("retry = %+v fresh=%v, want an existing uncertain receipt", receipt, fresh)
	}
}

func mismatchConflicts(t *testing.T, backend Backend) {
	store := backend.Open(t)
	begin(t, store, "k", "alice", "cancel run-1")
	for _, request := range []struct{ actor, payload string }{{"bob", "cancel run-1"}, {"alice", "cancel run-2"}} {
		if _, fresh, err := store.Begin(t.Context(), "k", request.actor, []byte(request.payload)); !errors.Is(err, cancelreceipt.ErrConflict) || fresh {
			t.Fatalf("Begin(%+v) = fresh %v, %v; want ErrConflict", request, fresh, err)
		}
	}
}

func finishOnce(t *testing.T, backend Backend) {
	store := backend.Open(t)
	begin(t, store, "k", "alice", "cancel")
	if err := store.Finish(t.Context(), "k", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	for _, result := range []string{"ok", "other"} {
		if err := store.Finish(t.Context(), "k", []byte(result)); !errors.Is(err, cancelreceipt.ErrTransition) {
			t.Fatalf("second Finish(%q) = %v, want ErrTransition", result, err)
		}
	}
	receipt, fresh := begin(t, store, "k", "alice", "cancel")
	if fresh || !receipt.Complete || !bytes.Equal(receipt.Result, []byte("ok")) {
		t.Fatalf("completed retry = %+v fresh=%v, want the first result", receipt, fresh)
	}
}

func finishUnknown(t *testing.T, backend Backend) {
	store := backend.Open(t)
	if err := store.Finish(t.Context(), "missing", []byte("ok")); !errors.Is(err, cancelreceipt.ErrTransition) {
		t.Fatalf("Finish of unknown key = %v, want ErrTransition", err)
	}
	begin(t, store, "k", "alice", "cancel")
	if err := store.Finish(t.Context(), "k", nil); !errors.Is(err, cancelreceipt.ErrTransition) {
		t.Fatalf("empty result = %v, want ErrTransition", err)
	}
	if err := store.Finish(t.Context(), "k", make([]byte, cancelreceipt.MaxBytes+1)); !errors.Is(err, cancelreceipt.ErrTransition) {
		t.Fatalf("oversized result = %v, want ErrTransition", err)
	}
}

func invalidRequests(t *testing.T, backend Backend) {
	store := backend.Open(t)
	for name, request := range map[string]struct {
		key, actor string
		payload    []byte
	}{
		"empty-key":     {"", "alice", []byte("x")},
		"padded-key":    {" k", "alice", []byte("x")},
		"control-key":   {"k\n", "alice", []byte("x")},
		"long-key":      {strings.Repeat("k", 201), "alice", []byte("x")},
		"long-actor":    {"k", strings.Repeat("a", 1025), []byte("x")},
		"empty-payload": {"k", "alice", nil},
		"long-payload":  {"k", "alice", make([]byte, cancelreceipt.MaxBytes+1)},
	} {
		if _, fresh, err := store.Begin(t.Context(), request.key, request.actor, request.payload); err == nil || fresh {
			t.Fatalf("%s: Begin accepted an invalid request (fresh=%v)", name, fresh)
		}
	}
	if _, fresh := begin(t, store, "k", "alice", "x"); !fresh {
		t.Fatal("rejected requests reserved the key")
	}
}

func durableAcrossReopen(t *testing.T, backend Backend) {
	begin(t, backend.Open(t), "uncertain", "alice", "a")
	first := backend.Open(t)
	begin(t, first, "done", "alice", "b")
	if err := first.Finish(t.Context(), "done", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	reopened := backend.Open(t)
	if receipt, fresh := begin(t, reopened, "uncertain", "alice", "a"); fresh || receipt.Complete {
		t.Fatalf("uncertain receipt after reopen = %+v fresh=%v", receipt, fresh)
	}
	if receipt, fresh := begin(t, reopened, "done", "alice", "b"); fresh || !receipt.Complete || string(receipt.Result) != "ok" {
		t.Fatalf("completed receipt after reopen = %+v fresh=%v", receipt, fresh)
	}
	if err := reopened.Finish(t.Context(), "uncertain", []byte("late")); err != nil {
		t.Fatalf("finishing an uncertain receipt after reopen: %v", err)
	}
}

func concurrentBegin(t *testing.T, backend Backend) {
	store := backend.Open(t)
	const callers = 4
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for range callers {
		wg.Go(func() {
			_, fresh, err := store.Begin(context.WithoutCancel(t.Context()), "same", "alice", []byte("cancel"))
			if err != nil {
				t.Error(err)
			}
			if fresh {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d fresh reservations, want exactly 1", winners)
	}
}
