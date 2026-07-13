package journal

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

const canary = "SUPER-SECRET-CANARY-9f8e7d6c5b4a3210"

// filesContaining walks a run dir and returns every file whose bytes contain
// needle — the direct test that no secret material landed at rest.
func filesContaining(t *testing.T, dir string, needle []byte) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(b, needle) {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return hits
}

// TestCanaryNeverLandsAtRest feeds a known token into an event, an input
// snapshot, AND an artifact, then asserts the raw canary appears in no journal
// file and that artifact digests verify post-scrub (they commit to the scrubbed
// bytes).
func TestCanaryNeverLandsAtRest(t *testing.T) {
	reg, scrub := DefaultScrubber()
	reg.Register([]byte(canary)) // as the resolver would, for an issued credential

	root := t.TempDir()
	run, err := Create(root, testIdentity(), map[string][]byte{
		"secret-input.md": []byte("here is the token " + canary + " embedded in an input"),
	}, WithScrubber(scrub), WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Canary inside an event (a runner annotation and an error message).
	if err := run.Append(Event{
		Type:   EventError,
		Error:  &ErrorDetail{Code: "leak", Message: "stage logged " + canary},
		Runner: map[string]any{"note": "token=" + canary},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Canary inside an artifact.
	art, err := run.RecordArtifact("leaky.log", []byte("BEGIN\n"+canary+"\nEND"))
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	_ = run.Close()

	dir := filepath.Join(root, testIdentity().RunID)
	if hits := filesContaining(t, dir, []byte(canary)); len(hits) > 0 {
		t.Fatalf("canary leaked to rest in: %v", hits)
	}

	// Artifact digest commits to scrubbed bytes and verifies on read.
	rd, _ := OpenRead(dir)
	got, err := rd.ArtifactBytes(art)
	if err != nil {
		t.Fatalf("ArtifactBytes: %v", err)
	}
	if bytes.Contains(got, []byte(canary)) {
		t.Fatalf("scrubbed artifact still contains canary")
	}
	if !bytes.Contains(got, []byte(Redacted)) {
		t.Fatalf("artifact was not redacted: %q", got)
	}
	if art.Digest != Digest(got) {
		t.Fatalf("digest does not commit to scrubbed bytes: %s vs %s", art.Digest, Digest(got))
	}
}

// TestPatternNetCatchesUnregistered proves the defense-in-depth pattern net
// redacts secret-shaped material that was never registered.
func TestPatternNetCatchesUnregistered(t *testing.T) {
	token := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789AB"
	scrub := NewPatternScrubber()
	out := scrub.Scrub([]byte("Authorization: token " + token))
	if bytes.Contains(out, []byte(token)) {
		t.Fatalf("pattern net missed a GitHub token: %q", out)
	}
	if !bytes.Contains(out, []byte(Redacted)) {
		t.Fatalf("expected redaction placeholder: %q", out)
	}
}

// TestRegistryLongestFirst ensures a secret containing a shorter registered
// value is fully redacted (longest-match-first), not partially unmasked.
func TestRegistryLongestFirst(t *testing.T) {
	reg := NewRegistryScrubber()
	reg.Register([]byte("abcdef"))
	reg.Register([]byte("abcdef-ghijkl-longer"))
	out := reg.Scrub([]byte("value=abcdef-ghijkl-longer end"))
	if bytes.Contains(out, []byte("abcdef")) {
		t.Fatalf("partial unmasking: %q", out)
	}
}

// TestRegistryIgnoresTinyValues ensures the registry does not redact trivially
// short values that would corrupt unrelated content.
func TestRegistryIgnoresTinyValues(t *testing.T) {
	reg := NewRegistryScrubber()
	reg.Register([]byte("ab")) // below minSecretLen; ignored
	out := reg.Scrub([]byte("a fabulous absolute cab"))
	if !bytes.Equal(out, []byte("a fabulous absolute cab")) {
		t.Fatalf("tiny value corrupted content: %q", out)
	}
}
