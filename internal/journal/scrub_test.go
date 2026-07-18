package journal

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const canary = "SUPER-SECRET-CANARY-9f8e7d6c5b4a3210"
const basicAuthCredential = "YnVpbGQtYWdlbnQ6YWRvLXBhdC0wMTIzNDU2Nzg5"

// escCanary contains characters JSON-escapes — a quote, a backslash, and the
// HTML-escaped <, >, & — so once an event is marshaled the secret appears ONLY
// in its escaped form. This is the #114/SEC-041 leak the raw-bytes registry
// missed: it searched for the literal secret, which the marshaled line no longer
// contains.
const escCanary = `ESC-CANARY-"<>&\-9f8e7d6c5b4a3210`

// jsonEscapedInner returns the JSON-string encoding of s without its surrounding
// quotes — the byte sequence s becomes as a marshaled field value.
func jsonEscapedInner(t *testing.T, s string) []byte {
	t.Helper()
	enc, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal %q: %v", s, err)
	}
	return enc[1 : len(enc)-1]
}

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

// TestCanaryIsMechanismIsolating is the negative control TestCanaryNeverLands
// AtRest depends on: it proves the pattern net alone does NOT catch `canary`,
// so a pass below actually certifies the registry-backed scrubber (the
// mechanism under test), not the pattern net catching a coincidentally
// recognizable secret shape. A canary shaped like a real provider token
// (ghp_..., AKIA..., etc.) would pass TestCanaryNeverLandsAtRest even if the
// registry wiring were entirely broken — false assurance on exactly the path
// that test exists to certify.
func TestCanaryIsMechanismIsolating(t *testing.T) {
	out := NewPatternScrubber().Scrub([]byte(canary))
	if string(out) != canary {
		t.Fatalf("pattern net alone redacts the canary (%q -> %q); it no longer isolates the "+
			"registry-backed scrubber TestCanaryNeverLandsAtRest is meant to certify — pick a canary "+
			"with no recognizable secret shape", canary, out)
	}
}

// TestCanaryNeverLandsAtRest feeds a known token into an event, an input
// snapshot, AND an artifact, then asserts the raw canary appears in no journal
// file and that artifact digests verify post-scrub (they commit to the scrubbed
// bytes). Mechanism-isolation for this canary is asserted separately by
// TestCanaryIsMechanismIsolating — a pass here certifies the registry path,
// not the pattern net.
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

// TestRegistryRedactsJSONEscapedSecret is the focused negative control for #114:
// a secret with escaped characters, embedded in a marshaled event line, is
// redacted in its escaped form. Against the raw-only registry this fails — the
// escaped secret survives.
func TestRegistryRedactsJSONEscapedSecret(t *testing.T) {
	reg := NewRegistryScrubber()
	reg.Register([]byte(escCanary))

	// Marshal exactly as appendEvent does (default json.Marshal, HTML escaping on).
	line, err := json.Marshal(map[string]string{"message": "token " + escCanary + " leaked"})
	if err != nil {
		t.Fatal(err)
	}
	// Precondition: the raw secret is NOT present in the marshaled bytes — it was
	// escaped — so the test genuinely exercises the escaped path.
	if bytes.Contains(line, []byte(escCanary)) {
		t.Fatal("test setup: expected the secret to be JSON-escaped in the marshaled line")
	}

	out := reg.Scrub(line)
	if bytes.Contains(out, jsonEscapedInner(t, escCanary)) {
		t.Fatalf("JSON-escaped secret survived scrubbing: %s", out)
	}
	// The distinctive prefix is part of the secret, so it must be gone too.
	if bytes.Contains(out, []byte("ESC-CANARY-")) {
		t.Fatalf("secret material survived scrubbing: %s", out)
	}
	if !bytes.Contains(out, []byte(Redacted)) {
		t.Fatalf("expected a redaction placeholder: %s", out)
	}
}

// TestJSONEscapedSecretNeverLandsAtRest is the end-to-end negative control for
// #114: a registered secret containing JSON-escaped characters, written into an
// event, must appear at rest in NO form — neither raw nor JSON-escaped.
func TestJSONEscapedSecretNeverLandsAtRest(t *testing.T) {
	reg, scrub := DefaultScrubber()
	reg.Register([]byte(escCanary))

	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithScrubber(scrub), WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := run.Append(Event{
		Type:   EventError,
		Error:  &ErrorDetail{Code: "leak", Message: "stage logged " + escCanary},
		Runner: map[string]any{"note": "token=" + escCanary},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	_ = run.Close()

	dir := filepath.Join(root, testIdentity().RunID)
	if hits := filesContaining(t, dir, []byte(escCanary)); len(hits) > 0 {
		t.Fatalf("raw escaped-secret leaked to rest in: %v", hits)
	}
	if hits := filesContaining(t, dir, jsonEscapedInner(t, escCanary)); len(hits) > 0 {
		t.Fatalf("JSON-escaped secret leaked to rest in: %v", hits)
	}
	if hits := filesContaining(t, dir, []byte("ESC-CANARY-")); len(hits) > 0 {
		t.Fatalf("secret material leaked to rest in: %v", hits)
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

func TestPatternNetRedactsBasicAuth(t *testing.T) {
	scrub := NewPatternScrubber()

	for _, tc := range []struct {
		header string
		want   string
	}{
		{"Authorization: Basic " + basicAuthCredential, "Authorization: " + Redacted},
		{"authorization: bAsIc " + basicAuthCredential, "authorization: " + Redacted},
	} {
		if got := string(scrub.Scrub([]byte(tc.header))); got != tc.want {
			t.Fatalf("scrubbed Basic authorization header = %q, want %q", got, tc.want)
		}
	}

	ordinary := []byte("artifact digest: " + basicAuthCredential)
	if got := scrub.Scrub(ordinary); !bytes.Equal(got, ordinary) {
		t.Fatalf("ordinary base64-looking text was redacted: %q", got)
	}
}

func TestBasicAuthNeverLandsInRunJournal(t *testing.T) {
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := run.Append(Event{
		Type:  EventError,
		Error: &ErrorDetail{Code: "provider_error", Message: "request failed with Authorization: Basic " + basicAuthCredential},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dir := filepath.Join(root, testIdentity().RunID)
	if hits := filesContaining(t, dir, []byte(basicAuthCredential)); len(hits) > 0 {
		t.Fatalf("Basic credential leaked into run journal: %v", hits)
	}
	rd, err := OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if got := events[len(events)-1].Error.Message; !strings.Contains(got, Redacted) {
		t.Fatalf("run journal error was not redacted: %q", got)
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
