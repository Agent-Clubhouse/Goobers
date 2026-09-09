package podauth

import (
	"bytes"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/readservice"
)

// This test only addresses the mutation plane; accidental read-plane use must
// not silently succeed through a fake response.
type checkpointUnusedReader struct{ readservice.Reader }

func TestSignedJournalTokenPublishesDurableTranscriptWithoutBlobPlane(t *testing.T) {
	t.Setenv("GOOBERS_DISABLE_FSYNC", "0")
	const runID = "checkpoint-http-run"
	runs := t.TempDir()
	run, err := journal.Create(runs, journal.RunIdentity{RunID: runID, Gaggle: "web", Workflow: "test",
		WorkflowVersion: 1, WorkflowDigest: "sha256:abc", Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) { return runs, gaggle == "web" },
		livejournal.WithScrubber(journal.NewPatternScrubber()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	key := testSignedKey(t)
	token, err := key.MintScoped(runID, time.Hour, ScopeJournal)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(checkpointUnusedReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(newTestAuthenticator(t, key)), httpapi.WithJournalService(writer))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	emitter := &livejournal.HTTPEmitter{BaseURL: server.URL, Token: token}
	transport := livejournal.TranscriptTransport{RunID: runID, Gaggle: "web", Emitter: emitter}
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte("private-credential"))
	session, err := transport.OpenTranscriptCheckpoint("build", "copilot-cli.transcript", scrubber)
	if err != nil {
		t.Fatal(err)
	}
	parts := []string{"working private-cred", "ential\n"}
	offset := 0
	for _, part := range parts {
		if err := session.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Offset: offset,
			Data: []byte(part), Reason: "checkpoint"}); err != nil {
			t.Fatal(err)
		}
		offset += len(part)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(runs, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var partial []byte
	for _, event := range events {
		if event.Runner["partial"] == true {
			data, err := reader.SpanBytes(*event.Ref)
			if err != nil {
				t.Fatal(err)
			}
			partial = append(partial, data...)
		}
	}
	if string(partial) != "working "+journal.Redacted+"\n" {
		t.Fatalf("acknowledged partial = %q", partial)
	}
	other := transport
	other.RunID = "other-run"
	if _, err := other.OpenTranscriptCheckpoint("build", "copilot-cli.transcript", scrubber); err == nil {
		t.Fatal("journal-only token wrote across run boundary")
	}
	if _, err := os.Stat(filepath.Join(runs, other.RunID)); !os.IsNotExist(err) {
		t.Fatalf("cross-run request created journal: %v", err)
	}
	// Base64 encoding exceeds the ordinary 4 MiB batch cap. No blob service or
	// blob scope is configured: these bytes must use the bounded inline path.
	final := []byte(strings.Repeat("finished\n", 500000))
	ref, err := session.RecordFinal("", final)
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	recovered, _, err := journal.Recover(filepath.Join(runs, runID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := recovered.Close(); err != nil {
			t.Error(err)
		}
	})
	got, err := reader.SpanBytes(ref)
	if err != nil || !bytes.Equal(got, final) {
		t.Fatalf("final not recoverable: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(runs, runID, "spans", "checkpoints"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("final did not retire partials: %v", err)
	}
}
