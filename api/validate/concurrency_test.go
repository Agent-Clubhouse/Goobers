package validate

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/goobers/goobers/api/schemas"
)

// concurrency_test.go pins #3887: a Validator is shared process-wide (the
// engine's verdict validator is a sync.OnceValues singleton, and the harness
// and agentkit builders keep one per component), so its lazy schema
// compilation has to be safe for concurrent use. Before the fix the cold-cache
// path wrote an unsynchronized map and drove a shared jsonschema.Compiler from
// every caller at once, which under -race reports a data race and in
// production can fatal the worker with "concurrent map writes".
//
// Both tests below are cold-cache by construction — each builds a FRESH
// Validator, so every goroutine reaches the compile path rather than a warm
// entry — and they must be run under -race to mean anything.

// verdictDoc is the smallest document the verdict schema accepts; the point of
// these tests is the compile/cache seam, not the schema's own verdicts.
func verdictDoc(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"decision": "pass", "summary": "ok"})
	if err != nil {
		t.Fatalf("marshal verdict: %v", err)
	}
	return data
}

// The engine's shape exactly: many concurrent placed-gate reviews validating
// the SAME envelope against a validator whose cache is still empty.
func TestValidatorToleratesConcurrentColdCacheEnvelopeValidation(t *testing.T) {
	const goroutines = 32
	v, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	doc := verdictDoc(t)

	start := make(chan struct{})
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- v.ValidateEnvelope("verdict", doc)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent ValidateEnvelope: %v", err)
		}
	}
}

// The same seam across DIFFERENT schema files, which is what actually drives
// the shared jsonschema.Compiler concurrently: one compile per file, all in
// flight at once, with the cross-schema $refs (invocation -> result ->
// artifact) resolving underneath.
func TestValidatorToleratesConcurrentColdCacheCompilesAcrossSchemas(t *testing.T) {
	files := make([]string, 0, len(schemas.Envelope))
	for _, file := range schemas.Envelope {
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatal("schemas.Envelope is empty; the test would assert nothing")
	}

	v, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const perFile = 8
	start := make(chan struct{})
	errs := make(chan error, len(files)*perFile)
	var wg sync.WaitGroup
	for _, file := range files {
		for i := 0; i < perFile; i++ {
			wg.Add(1)
			go func(file string) {
				defer wg.Done()
				<-start
				// schema() is the unsynchronized seam itself: compile once,
				// then serve from the cache.
				if _, err := v.schema(file); err != nil {
					errs <- err
				}
			}(file)
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent schema compile: %v", err)
	}

	// Every file resolved to exactly one cached schema, and a second read
	// returns the identical pointer — the mutex must not turn the cache into
	// a per-call recompile.
	for _, file := range files {
		first, err := v.schema(file)
		if err != nil {
			t.Fatalf("schema(%s): %v", file, err)
		}
		second, err := v.schema(file)
		if err != nil {
			t.Fatalf("schema(%s): %v", file, err)
		}
		if first != second {
			t.Fatalf("schema(%s) recompiled on a warm cache", file)
		}
	}
}
