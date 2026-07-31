package conformance_test

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/conformance"
)

// TestEmbeddedBackendSatisfiesTheContract runs the backend-neutral contract
// against the embedded SQLite store.
//
// This is the acceptance criterion for #1921: the contract is expressed without
// SQLite specifics and passes against the embedded backend. When a second
// backend arrives (#652), it gets its own file exactly like this one and shares
// every assertion.
func TestEmbeddedBackendSatisfiesTheContract(t *testing.T) {
	var n int
	conformance.Run(t, func(t *testing.T) readmodel.Backend {
		// A fresh file per call, because several cases need two independent
		// stores at once — the epoch case is unwritable otherwise.
		n++
		store, err := readmodel.Open(filepath.Join(t.TempDir(), "read.db"))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	})
}
