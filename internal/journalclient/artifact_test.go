package journalclient

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// artifact_test.go covers the typed artifact-content fetch's own rules: what
// it will resolve, what it refuses before it fetches anything, and what it
// refuses after. The backend-specific halves live beside them —
// journalclient_test.go for the HTTP transport's bounds, and
// internal/httpapi/journalreadparity_test.go for the claim that both backends
// answer identically.

// stubReader is a Reader whose event log and blob store are supplied by the
// test, so a journal shape that a real writer would never produce — a ref
// pointing outside the run, a size that disagrees with the bytes — can be put
// in front of the fetch.
type stubReader struct {
	runID  string
	events []journal.Event
	blobs  map[string][]byte
	// served overrides the bytes returned for a digest, modelling a backend
	// that answers with content other than what was asked for.
	served []byte
	reads  int
}

func (s *stubReader) RunID() string { return s.runID }

func (s *stubReader) Events() ([]journal.Event, error) { return s.events, nil }

func (s *stubReader) ArtifactBytes(ref journal.Ref) ([]byte, error) {
	return s.ArtifactBytesBounded(ref, 0)
}

func (s *stubReader) ArtifactBytesBounded(ref journal.Ref, maxBytes int64) ([]byte, error) {
	s.reads++
	data := s.served
	if data == nil {
		var ok bool
		data, ok = s.blobs[ref.Digest]
		if !ok {
			return nil, fmt.Errorf("stub: no blob for %s", ref.Digest)
		}
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("stub: blob exceeds the %d byte ceiling", maxBytes)
	}
	return data, nil
}

func (s *stubReader) ArtifactByDigest(digest string) ([]byte, error) {
	return s.ArtifactBytesBounded(journal.Ref{Digest: digest}, 0)
}

func (s *stubReader) StageAttempts(string) ([]StageAttempt, error) { return nil, nil }

func (s *stubReader) Phase() (journal.RunPhase, error) { return "", nil }

var _ Reader = (*stubReader)(nil)

// seedStub builds a reader whose journal records content as stage's stdout
// artifact, exactly as the executor does: an artifact.recorded event under
// "<task>/stdout.log", declared by a successful stage.finished with the
// text/plain pointer the executor attaches.
func seedStub(t *testing.T, stage string, content []byte) (*stubReader, journal.Ref) {
	t.Helper()
	ref, err := journal.ArtifactRef(content)
	if err != nil {
		t.Fatalf("artifact ref: %v", err)
	}
	reader := &stubReader{
		runID: "run-1",
		blobs: map[string][]byte{ref.Digest: content},
		events: []journal.Event{
			{Type: journal.EventStageStarted, Stage: stage, Attempt: 1},
			{Type: journal.EventArtifactRecorded, Stage: stage, Attempt: 1, Name: "run-1:" + stage + "/stdout.log", Ref: &ref},
			{
				Type: journal.EventStageFinished, Stage: stage, Attempt: 1,
				Status:    string(apiv1.ResultSuccess),
				Artifacts: []journal.Ref{{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: "text/plain"}},
			},
		},
	}
	return reader, ref
}

// TestStageArtifactContentServesTheRecordedArtifact is the happy path, and it
// pins what the typed answer carries: content the caller can act on, plus the
// facts the journal recorded about it, and no host path anywhere.
func TestStageArtifactContentServesTheRecordedArtifact(t *testing.T) {
	content := []byte("=== go vet ===\ninternal/worktree/manager.go:88: nope\n")
	reader, ref := seedStub(t, "collect-repo-signals", content)

	got, err := StageArtifactContent(reader, "collect-repo-signals", "/stdout.log", ArtifactBounds{
		MaxBytes: 1 << 20, MediaTypes: []string{"text/plain"},
	})
	if err != nil {
		t.Fatalf("StageArtifactContent: %v", err)
	}
	if string(got.Bytes) != string(content) {
		t.Fatalf("bytes = %q, want %q", got.Bytes, content)
	}
	if got.Digest != ref.Digest || got.Size != int64(len(content)) {
		t.Fatalf("content = %+v, want digest %s and size %d", got, ref.Digest, len(content))
	}
	if got.Stage != "collect-repo-signals" || got.Name != "run-1:collect-repo-signals/stdout.log" {
		t.Fatalf("content = %+v, want the recorded stage and name", got)
	}
	if got.MediaType != "text/plain" {
		t.Fatalf("media type = %q, want the declared text/plain", got.MediaType)
	}
}

// TestStageArtifactContentRefusesAnUnrecordedArtifact covers every way the
// journal can fail to bind one: no such stage, a stage that failed, an
// artifact recorded by a DIFFERENT stage, and a name that does not match. All
// of them are one explicit error — never an empty body a caller could mistake
// for "the tool found nothing".
func TestStageArtifactContentRefusesAnUnrecordedArtifact(t *testing.T) {
	content := []byte("signals")
	ref, err := journal.ArtifactRef(content)
	if err != nil {
		t.Fatal(err)
	}
	pointer := journal.Ref{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: "text/plain"}

	cases := map[string][]journal.Event{
		"empty journal": {},
		"stage never finished": {
			{Type: journal.EventArtifactRecorded, Name: "run-1:signals/stdout.log", Ref: &ref},
		},
		"stage finished unsuccessfully": {
			{Type: journal.EventArtifactRecorded, Name: "run-1:signals/stdout.log", Ref: &ref},
			{Type: journal.EventStageFinished, Stage: "signals", Status: string(apiv1.ResultFailure), Artifacts: []journal.Ref{pointer}},
		},
		"another stage declared it": {
			{Type: journal.EventArtifactRecorded, Name: "run-1:gather-telemetry/stdout.log", Ref: &ref},
			{Type: journal.EventStageFinished, Stage: "gather-telemetry", Status: string(apiv1.ResultSuccess), Artifacts: []journal.Ref{pointer}},
		},
		"name does not match": {
			{Type: journal.EventArtifactRecorded, Name: "run-1:signals/stderr.log", Ref: &ref},
			{Type: journal.EventStageFinished, Stage: "signals", Status: string(apiv1.ResultSuccess), Artifacts: []journal.Ref{pointer}},
		},
		"declared but never recorded": {
			{Type: journal.EventStageFinished, Stage: "signals", Status: string(apiv1.ResultSuccess), Artifacts: []journal.Ref{pointer}},
		},
	}
	for name, events := range cases {
		t.Run(name, func(t *testing.T) {
			reader := &stubReader{runID: "run-1", events: events, blobs: map[string][]byte{ref.Digest: content}}
			_, err := StageArtifactContent(reader, "signals", "/stdout.log", ArtifactBounds{})
			if !errors.Is(err, ErrArtifactNotRecorded) {
				t.Fatalf("err = %v, want ErrArtifactNotRecorded", err)
			}
			if reader.reads != 0 {
				t.Fatalf("%d blob reads were attempted for an unrecorded artifact", reader.reads)
			}
		})
	}
}

// TestStageArtifactContentRefusesAPathThatIsNotItsOwnDigest is the
// containment property, and it is the reason the fetch takes a resolved
// reference rather than a path: a journal whose recorded path has been
// steered somewhere else — the classic ../../ traversal, or simply another
// artifact's shard — is refused outright, and nothing is read.
func TestStageArtifactContentRefusesAPathThatIsNotItsOwnDigest(t *testing.T) {
	content := []byte("signals")
	honest, err := journal.ArtifactRef(content)
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"traversal":         "../../../../etc/passwd",
		"escapes the tree":  "artifacts/sha256/../../../secrets",
		"another shard":     "artifacts/sha256/ff/" + strings.Repeat("f", 62),
		"outside artifacts": "spans/sha256/" + strings.TrimPrefix(honest.Path, "artifacts/sha256/"),
	} {
		t.Run(name, func(t *testing.T) {
			tampered := honest
			tampered.Path = path
			reader := &stubReader{
				runID: "run-1",
				blobs: map[string][]byte{honest.Digest: content},
				events: []journal.Event{
					{Type: journal.EventArtifactRecorded, Name: "run-1:signals/stdout.log", Ref: &tampered},
					{
						Type: journal.EventStageFinished, Stage: "signals", Status: string(apiv1.ResultSuccess),
						Artifacts: []journal.Ref{{Path: path, Digest: honest.Digest, Size: honest.Size, MediaType: "text/plain"}},
					},
				},
			}
			_, err := StageArtifactContent(reader, "signals", "/stdout.log", ArtifactBounds{})
			if !errors.Is(err, ErrArtifactUnsafeRef) {
				t.Fatalf("err = %v, want ErrArtifactUnsafeRef", err)
			}
			if reader.reads != 0 {
				t.Fatalf("%d blob reads were attempted for an unsafe reference", reader.reads)
			}
		})
	}
}

// TestStageArtifactContentRefusesAMalformedDigest keeps a reference that is
// not a content address from ever addressing a read.
func TestStageArtifactContentRefusesAMalformedDigest(t *testing.T) {
	for _, digest := range []string{"", "notadigest", "sha256:short", "md5:" + strings.Repeat("a", 32)} {
		ref := journal.Ref{Digest: digest, Size: 3}
		reader := &stubReader{
			runID: "run-1",
			events: []journal.Event{
				{Type: journal.EventArtifactRecorded, Name: "run-1:signals/stdout.log", Ref: &ref},
				{
					Type: journal.EventStageFinished, Stage: "signals", Status: string(apiv1.ResultSuccess),
					Artifacts: []journal.Ref{ref},
				},
			},
		}
		if _, err := StageArtifactContent(reader, "signals", "/stdout.log", ArtifactBounds{}); !errors.Is(err, ErrArtifactUnsafeRef) {
			t.Errorf("digest %q: err = %v, want ErrArtifactUnsafeRef", digest, err)
		}
		if reader.reads != 0 {
			t.Errorf("digest %q: %d blob reads were attempted", digest, reader.reads)
		}
	}
}

// TestStageArtifactContentRefusesAnOversizedArtifact refuses on the RECORDED
// size, before any bytes are fetched: a stage that will only act on a bounded
// artifact must not be made to buffer an unbounded one first.
func TestStageArtifactContentRefusesAnOversizedArtifact(t *testing.T) {
	content := []byte(strings.Repeat("x", 4096))
	reader, _ := seedStub(t, "signals", content)

	_, err := StageArtifactContent(reader, "signals", "/stdout.log", ArtifactBounds{MaxBytes: 1024})
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("err = %v, want ErrArtifactTooLarge", err)
	}
	if reader.reads != 0 {
		t.Fatalf("%d blob reads were attempted for an oversized artifact", reader.reads)
	}

	// The same artifact inside the bound is served.
	if _, err := StageArtifactContent(reader, "signals", "/stdout.log", ArtifactBounds{MaxBytes: 8192}); err != nil {
		t.Fatalf("within-bound fetch: %v", err)
	}
}

// TestStageArtifactContentClampsToTheHardCeiling pins that a caller cannot
// raise the bound past what a stage pod can hold, and that asking for no
// bound is the ceiling rather than no limit at all.
func TestStageArtifactContentClampsToTheHardCeiling(t *testing.T) {
	for name, bounds := range map[string]ArtifactBounds{
		"unset":        {},
		"negative":     {MaxBytes: -1},
		"over the cap": {MaxBytes: MaxStageArtifactBytes * 4},
	} {
		if got := bounds.limit(); got != MaxStageArtifactBytes {
			t.Errorf("%s: limit = %d, want %d", name, got, MaxStageArtifactBytes)
		}
	}
	if got := (ArtifactBounds{MaxBytes: 1024}).limit(); got != 1024 {
		t.Errorf("limit = %d, want the caller's 1024", got)
	}
}

// TestStageArtifactContentRefusesTamperedContent is the integrity half: a
// backend that answers with different bytes, or with the right bytes at the
// wrong length, is refused rather than parsed. This is what stops a
// compromised daemon from feeding a filer a fabricated tool finding to
// approve a nomination against.
func TestStageArtifactContentRefusesTamperedContent(t *testing.T) {
	content := []byte("=== go vet ===\nreal finding\n")

	t.Run("substituted bytes", func(t *testing.T) {
		reader, _ := seedStub(t, "signals", content)
		reader.served = []byte("=== go vet ===\nfabricated finding\n")
		_, err := StageArtifactContent(reader, "signals", "/stdout.log", ArtifactBounds{})
		if !errors.Is(err, ErrArtifactIntegrity) {
			t.Fatalf("err = %v, want ErrArtifactIntegrity", err)
		}
	})

	t.Run("length disagrees with the recorded size", func(t *testing.T) {
		reader, ref := seedStub(t, "signals", content)
		// The bytes still digest correctly; the journal's recorded size does
		// not describe them. Something rewrote one of the two.
		inflated := ref
		inflated.Size = ref.Size + 512
		reader.events[1].Ref = &inflated
		reader.events[2].Artifacts[0].Size = inflated.Size
		_, err := StageArtifactContent(reader, "signals", "/stdout.log", ArtifactBounds{})
		if !errors.Is(err, ErrArtifactIntegrity) {
			t.Fatalf("err = %v, want ErrArtifactIntegrity", err)
		}
	})
}

// TestStageArtifactContentBoundsTheMediaType refuses an artifact that
// POSITIVELY declares a type the caller will not act on, and admits the
// unspecific declaration the daemon's projection is limited to — the property
// that keeps the two backends deciding alike (see ArtifactBounds.MediaTypes).
func TestStageArtifactContentBoundsTheMediaType(t *testing.T) {
	content := []byte("signals")

	t.Run("foreign declared type is refused", func(t *testing.T) {
		reader, _ := seedStub(t, "signals", content)
		reader.events[2].Artifacts[0].MediaType = "application/zip"
		_, err := StageArtifactContent(reader, "signals", "/stdout.log", ArtifactBounds{MediaTypes: []string{"text/plain"}})
		if !errors.Is(err, ErrArtifactMediaType) {
			t.Fatalf("err = %v, want ErrArtifactMediaType", err)
		}
		if reader.reads != 0 {
			t.Fatalf("%d blob reads were attempted past the media bound", reader.reads)
		}
	})

	for name, declared := range map[string]string{
		"absent":  "",
		"generic": GenericMediaType,
	} {
		t.Run(name+" declaration is admitted", func(t *testing.T) {
			reader, _ := seedStub(t, "signals", content)
			reader.events[2].Artifacts[0].MediaType = declared
			got, err := StageArtifactContent(reader, "signals", "/stdout.log", ArtifactBounds{MediaTypes: []string{"text/plain"}})
			if err != nil {
				t.Fatalf("StageArtifactContent: %v", err)
			}
			if got.MediaType != GenericMediaType {
				t.Fatalf("media type = %q, want the generic type", got.MediaType)
			}
		})
	}

	t.Run("parameters and case do not change the decision", func(t *testing.T) {
		reader, _ := seedStub(t, "signals", content)
		reader.events[2].Artifacts[0].MediaType = "Text/Plain; charset=utf-8"
		if _, err := StageArtifactContent(reader, "signals", "/stdout.log", ArtifactBounds{MediaTypes: []string{"text/plain; charset=utf-8"}}); err != nil {
			t.Fatalf("StageArtifactContent: %v", err)
		}
	})

	t.Run("an empty bound admits anything", func(t *testing.T) {
		reader, _ := seedStub(t, "signals", content)
		reader.events[2].Artifacts[0].MediaType = "application/zip"
		if _, err := StageArtifactContent(reader, "signals", "/stdout.log", ArtifactBounds{}); err != nil {
			t.Fatalf("StageArtifactContent: %v", err)
		}
	})
}

// TestStageArtifactContentTakesTheNewestSuccessfulAttempt pins the resolution
// order: a stage that ran twice is confirmed against what it produced LAST,
// not against a stale earlier capture.
func TestStageArtifactContentTakesTheNewestSuccessfulAttempt(t *testing.T) {
	first := []byte("first attempt output")
	second := []byte("second attempt output")
	firstRef, err := journal.ArtifactRef(first)
	if err != nil {
		t.Fatal(err)
	}
	secondRef, err := journal.ArtifactRef(second)
	if err != nil {
		t.Fatal(err)
	}
	reader := &stubReader{
		runID: "run-1",
		blobs: map[string][]byte{firstRef.Digest: first, secondRef.Digest: second},
		events: []journal.Event{
			{Type: journal.EventArtifactRecorded, Name: "run-1:signals/stdout.log", Ref: &firstRef},
			{
				Type: journal.EventStageFinished, Stage: "signals", Attempt: 1, Status: string(apiv1.ResultSuccess),
				Artifacts: []journal.Ref{{Path: firstRef.Path, Digest: firstRef.Digest, Size: firstRef.Size, MediaType: "text/plain"}},
			},
			{Type: journal.EventArtifactRecorded, Name: "run-1:signals/stdout.log", Ref: &secondRef},
			{
				Type: journal.EventStageFinished, Stage: "signals", Attempt: 2, Status: string(apiv1.ResultSuccess),
				Artifacts: []journal.Ref{{Path: secondRef.Path, Digest: secondRef.Digest, Size: secondRef.Size, MediaType: "text/plain"}},
			},
		},
	}
	got, err := StageArtifactContent(reader, "signals", "/stdout.log", ArtifactBounds{})
	if err != nil {
		t.Fatalf("StageArtifactContent: %v", err)
	}
	if string(got.Bytes) != string(second) {
		t.Fatalf("bytes = %q, want the newest attempt's %q", got.Bytes, second)
	}
}

// TestStageArtifactContentRefusesAnUnaskableQuestion keeps the call from
// degenerating into a general fetch: there is no way to ask for "any
// artifact", and no reader means no read.
func TestStageArtifactContentRefusesAnUnaskableQuestion(t *testing.T) {
	reader, _ := seedStub(t, "signals", []byte("signals"))
	for name, call := range map[string]func() error{
		"no reader": func() error {
			_, err := StageArtifactContent(nil, "signals", "/stdout.log", ArtifactBounds{})
			return err
		},
		"no stage": func() error {
			_, err := StageArtifactContent(reader, "  ", "/stdout.log", ArtifactBounds{})
			return err
		},
		"no name suffix": func() error {
			_, err := StageArtifactContent(reader, "signals", "", ArtifactBounds{})
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: the fetch succeeded, want a refusal", name)
		}
	}
}
