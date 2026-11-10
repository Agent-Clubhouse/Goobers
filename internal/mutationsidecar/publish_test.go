package mutationsidecar

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

type lostReceiptAck struct{ writer *livejournal.Writer }

func (e lostReceiptAck) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	response, err := e.writer.Emit(ctx, req)
	if err == nil {
		err = errors.New("response lost after durable journal append")
	}
	return response, err
}

func TestReceiptPublicationRecoversLostAckAfterHostRestart(t *testing.T) {
	runs, workspace := t.TempDir(), t.TempDir()
	const owner = "source-run"
	run, err := journal.Create(runs, journal.RunIdentity{RunID: owner, Gaggle: "gaggle", Workflow: "implementation", WorkflowVersion: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"receiptId":"receipt-1","provider":"github","kind":"pr","id":"42","operation":"merge"}`)
	path := filepath.Join(workspace, "mutations.jsonl")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	newWriter := func() *livejournal.Writer {
		writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) { return runs, gaggle == "gaggle" })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(writer.Close)
		return writer
	}
	writer := newWriter()
	scrubber := journal.NewPatternScrubber()
	if err := PublishBeforeCleanup(t.Context(), workspace, "source-land", owner, "gaggle", scrubber, lostReceiptAck{writer}); err == nil {
		t.Fatal("lost ACK authorized cleanup")
	}
	writer.Close()
	writer = newWriter()
	if err := PublishBeforeCleanup(t.Context(), workspace, "source-land", owner, "gaggle", scrubber, writer); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(runs, owner))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Runner["mutationReceiptId"] == "receipt-1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("retry retained %d copies of one receipt", count)
	}
	// Local cleanup recognizes the same proof without borrowing the live
	// writer lock or trusting only the transport ACK.
	if err := RecoverBeforeCleanup(t.Context(), workspace, "source-land", owner, filepath.Join(runs, owner)); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
		t.Fatalf("publication modified source: %q %v", got, err)
	}
}

type stubScrubber struct{ out []byte }

func (s stubScrubber) Scrub(in []byte) []byte {
	if s.out != nil {
		return s.out
	}
	return in
}

type stubEmitter struct {
	resp  livejournal.EmitResponse
	err   error
	calls int
}

func (e *stubEmitter) Emit(context.Context, livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	e.calls++
	return e.resp, e.err
}

func TestPublishBeforeCleanupFailsClosed(t *testing.T) {
	const good = `{"receiptId":"receipt-1","provider":"github","kind":"pr","id":"42","operation":"merge"}`
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	ok := livejournal.EmitResponse{Applied: 1, Seq: 1}
	tests := []struct {
		name       string
		sidecar    string
		dir        bool
		ctx        context.Context
		runID      string
		worktreeID string
		scrubber   journal.Scrubber
		resp       livejournal.EmitResponse
		emitErr    error
		nilEmitter bool
		wantCalls  int
	}{
		{name: "unreadable sidecar", dir: true},
		{name: "malformed sidecar", sidecar: "{not json"},
		{name: "unknown field", sidecar: `{"provider":"github","kind":"pr","id":"1","bogus":true}`},
		{name: "invalid run id", sidecar: good, runID: "../bad"},
		{name: "empty worktree id", sidecar: good, worktreeID: "-"},
		{name: "nil scrubber", sidecar: good, scrubber: nilScrubber{}},
		{name: "nil emitter", sidecar: good, nilEmitter: true},
		{name: "legacy receipt without identity", sidecar: `{"provider":"github","kind":"pr","id":"42"}`},
		{name: "canceled context", sidecar: good, ctx: canceled},
		{name: "scrubber changes receipt", sidecar: good, scrubber: stubScrubber{out: []byte("redacted")}},
		{name: "emit error", sidecar: good, emitErr: errors.New("host down"), wantCalls: 1},
		{name: "negative applied", sidecar: good, resp: livejournal.EmitResponse{Applied: -1, Deduplicated: 2, Seq: 1}, wantCalls: 1},
		{name: "negative deduplicated", sidecar: good, resp: livejournal.EmitResponse{Applied: 2, Deduplicated: -1, Seq: 1}, wantCalls: 1},
		{name: "nothing acknowledged", sidecar: good, resp: livejournal.EmitResponse{Seq: 1}, wantCalls: 1},
		{name: "overcounted ack", sidecar: good, resp: livejournal.EmitResponse{Applied: 1, Deduplicated: 1, Seq: 1}, wantCalls: 1},
		{name: "zero seq", sidecar: good, resp: livejournal.EmitResponse{Applied: 1}, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, FileName)
			if tt.dir {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(tt.sidecar), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, runID, worktreeID, scrubber := tt.ctx, tt.runID, tt.worktreeID, tt.scrubber
			if ctx == nil {
				ctx = context.Background()
			}
			if runID == "" {
				runID = "source-run"
			}
			switch worktreeID {
			case "":
				worktreeID = "source-land"
			case "-":
				worktreeID = ""
			}
			if scrubber == nil {
				scrubber = stubScrubber{}
			} else if _, isNil := scrubber.(nilScrubber); isNil {
				scrubber = nil
			}
			resp := tt.resp
			if reflect.DeepEqual(resp, livejournal.EmitResponse{}) && tt.emitErr == nil && tt.wantCalls == 0 {
				resp = ok
			}
			emitter := &stubEmitter{resp: resp, err: tt.emitErr}
			var re ReceiptEmitter = emitter
			if tt.nilEmitter {
				re = nil
			}
			if err := PublishBeforeCleanup(ctx, workspace, worktreeID, runID, "gaggle", scrubber, re); err == nil {
				t.Fatal("expected fail-closed error")
			}
			if emitter.calls != tt.wantCalls {
				t.Fatalf("emit calls = %d, want %d", emitter.calls, tt.wantCalls)
			}
			if !tt.dir {
				if got, _ := os.ReadFile(path); string(got) != tt.sidecar {
					t.Fatalf("source modified: %q", got)
				}
			}
		})
	}
}

type nilScrubber struct{}

func (nilScrubber) Scrub(in []byte) []byte { return in }

func TestPublishBeforeCleanupNoSidecarOrFacts(t *testing.T) {
	for name, content := range map[string]*string{"missing": nil, "blank": new(string)} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			if content != nil {
				if err := os.WriteFile(filepath.Join(workspace, FileName), []byte("\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := PublishBeforeCleanup(context.Background(), workspace, "", "", "g", nil, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}
