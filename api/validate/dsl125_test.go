package validate

import (
	"strings"
	"testing"
)

const fakeDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestContextPointerRunIDValidatesAgainstSchema is #125 item 7: ContextPointer
// carries a runId in Go (cross-run evidence, #103/T3), but the closed invocation
// schema omitted it — so a valid cross-run envelope failed schema validation.
// With the schema reconciled, an envelope whose context pointer names another
// run validates.
func TestContextPointerRunIDValidatesAgainstSchema(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	envelope := `{
		"taskId": "t", "workflowId": "w", "runId": "this-run", "gaggle": "g",
		"goal": "resolve cross-run evidence", "workspace": "/ws",
		"repoRef": {"provider": "github", "owner": "o", "name": "n"},
		"limits": {},
		"contextPointers": [
			{"name": "evidence", "runId": "other-run",
			 "artifact": {"path": "findings/x.json", "digest": "` + fakeDigest + `"}}
		]
	}`
	if err := v.ValidateJSON("invocation.schema.json", []byte(envelope)); err != nil {
		t.Fatalf("cross-run contextPointer envelope should validate, got: %v", err)
	}
}

// TestArtifactPointerRejectsTraversalAtValidateTime is #125 item 8: the
// artifact-pointer schema previously rejected only a leading '/', deferring '..'
// containment to resolve time. It now rejects '..' path components too, so a
// foreign-authored envelope that would escape the journal fails at validate
// time. The schema guard is a conservative *superset* of the Go
// ResolveContainedPath check, not an exact match: it rejects every '..' segment,
// including the contained '..' that filepath.Clean collapses and
// ResolveContainedPath would accept (e.g. "a/../b" -> "a/b"). The divergence is
// always in the safe direction — validate is stricter than resolve, never more
// lenient — so no traversal that resolve rejects can pass validation.
func TestArtifactPointerRejectsTraversalAtValidateTime(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bad := []string{
		"../escape.json",
		"a/../../etc/passwd",
		"..",
		"findings/..",
		"/absolute/path.json",
		// filepath.Clean collapses "a/../b" -> "a/b", so Go's ResolveContainedPath
		// accepts it; the schema rejects any '..' segment outright. This documents
		// the intentional stricter-than-Go behavior (safe direction).
		"a/../b",
	}
	for _, p := range bad {
		doc := `{"path": "` + p + `", "digest": "` + fakeDigest + `"}`
		if err := v.ValidateJSON("artifact-pointer.schema.json", []byte(doc)); err == nil {
			t.Errorf("artifact-pointer path %q should be rejected by the schema, but validated", p)
		}
	}
	good := []string{
		"findings/x.json",
		"run-1:query-backlog/stdout.log",
		"a.b",
		"...", // three dots is a legit filename, not a traversal
	}
	for _, p := range good {
		doc := `{"path": "` + p + `", "digest": "` + fakeDigest + `"}`
		if err := v.ValidateJSON("artifact-pointer.schema.json", []byte(doc)); err != nil {
			t.Errorf("artifact-pointer path %q should validate, got: %v", p, err)
		}
	}
	// Sanity: the rejections above are about the path, not the digest.
	if !strings.HasPrefix(fakeDigest, "sha256:") {
		t.Fatal("test digest malformed")
	}
}
