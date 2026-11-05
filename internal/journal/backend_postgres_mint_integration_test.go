//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
)

func mintRequest() journal.CreateRunRequest {
	return journal.CreateRunRequest{Identity: journal.RunIdentity{RunID: "run-mint", Gaggle: "g", Workflow: "mint"}}
}

func assertMintAppend(t *testing.T, b *journalpg.Backend, h journal.RunHandle, owner journal.OwnerToken) {
	t.Helper()
	if h == "" || owner == "" {
		t.Fatal("success did not publish a writer")
	}
	if n, err := b.AppendBatch(t.Context(), h, owner, []journal.Event{{Type: journal.EventRunnerAnnotation, Name: "after-mint"}}); err != nil || n != 1 {
		t.Fatalf("append after mint: n=%d err=%v", n, err)
	}
	records, err := b.ReadEvents(t.Context(), journal.RunRef{ID: "run-mint"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, record := range records {
		if record.Event.Type == journal.EventRunStarted {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("run.started count=%d", starts)
	}
}

func TestPostgresUnknownCreateMint(t *testing.T) {
	for _, retry := range []string{"resolved", "create", "recover", "concurrent"} {
		t.Run(retry, func(t *testing.T) {
			b, _, fault := mintFaultBackend(t, false)
			fault.armed.Store(true)
			fault.failRead.Store(retry != "resolved")
			h, owner, err := b.CreateRun(t.Context(), mintRequest())
			fault.finish(t)
			if retry == "resolved" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, lease.ErrCommitUnknown) || h != "" || owner != "" {
					t.Fatalf("unresolved mint published authority: %q %q %v", h, owner, err)
				}
				switch retry {
				case "create":
					h, owner, err = b.CreateRun(t.Context(), mintRequest())
				case "recover":
					h, owner, _, err = b.RecoverRun(t.Context(), journal.RunKey{Gaggle: "g", RunID: "run-mint"}, journal.RunWriterOptions{})
				case "concurrent":
					h, owner = concurrentMintRecover(t, b)
					err = nil
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			assertMintAppend(t, b, h, owner)
		})
	}
}

func concurrentMintRecover(t *testing.T, b *journalpg.Backend) (journal.RunHandle, journal.OwnerToken) {
	t.Helper()
	type result struct {
		h     journal.RunHandle
		owner journal.OwnerToken
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			h, owner, _, err := b.RecoverRun(t.Context(), journal.RunKey{Gaggle: "g", RunID: "run-mint"}, journal.RunWriterOptions{})
			results <- result{h, owner, err}
		}()
	}
	ready.Wait()
	close(start)
	winners := 0
	var winner result
	for range 2 {
		r := <-results
		if r.err == nil {
			winners++
			winner = r
		} else if !errors.Is(r.err, journal.ErrRunOwned) || r.h != "" || r.owner != "" {
			t.Fatalf("loser published authority or wrong error: %+v", r)
		}
	}
	if winners != 1 {
		t.Fatalf("published %d writers, want exactly one", winners)
	}
	return winner.h, winner.owner
}

func TestPostgresUncommittedCreateMintRetries(t *testing.T) {
	b, config, fault := mintFaultBackend(t, true)
	fault.armed.Store(true)
	h, owner, err := b.CreateRun(t.Context(), mintRequest())
	fault.finish(t)
	if err == nil || errors.Is(err, lease.ErrCommitUnknown) || h != "" || owner != "" {
		t.Fatalf("absent mint should be definitely retryable: %q %q %v", h, owner, err)
	}
	var count int
	if err := config.Pool.QueryRow(t.Context(), "SELECT count(*) FROM journal.run_seq WHERE run_id='run-mint'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("uncommitted row count=%d: %v", count, err)
	}
	h, owner, err = b.CreateRun(t.Context(), mintRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertMintAppend(t, b, h, owner)
}

func TestPostgresUnresolvedMintCannotAdoptForeignToken(t *testing.T) {
	b, _, fault := mintFaultBackend(t, false)
	fault.armed.Store(true)
	fault.failRead.Store(true)
	if _, _, err := b.CreateRun(t.Context(), mintRequest()); !errors.Is(err, lease.ErrCommitUnknown) {
		t.Fatal(err)
	}
	fault.finish(t)
	if _, err := fault.admin.Exec(t.Context(), "UPDATE journal.run_seq SET owner_token='foreign' WHERE run_id='run-mint'"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	h, owner, _, err := b.RecoverRun(ctx, journal.RunKey{Gaggle: "g", RunID: "run-mint"}, journal.RunWriterOptions{})
	if !errors.Is(err, journal.ErrRunOwned) || h != "" || owner != "" {
		t.Fatalf("foreign live token was adopted: %q %q %v", h, owner, err)
	}
}

func TestPostgresUnknownRecoveryMintCleansTranscript(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "resolved"
		if fallback {
			name = "unresolved"
		}
		t.Run(name, func(t *testing.T) {
			b, _, fault := mintFaultBackend(t, false)
			h, owner, err := b.CreateRun(t.Context(), mintRequest())
			if err != nil {
				t.Fatal(err)
			}
			capture, err := b.BeginTranscript(t.Context(), h, owner, 0, "stage", "transcript")
			if err != nil {
				t.Fatal(err)
			}
			if err := capture.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Data: []byte("partial\n"), Reason: "checkpoint"}); err != nil {
				t.Fatal(err)
			}
			records, err := b.ReadEvents(t.Context(), journal.RunRef{ID: "run-mint"}, 1, 0)
			if err != nil || len(records) != 1 || records[0].Event.Ref == nil {
				t.Fatalf("partial: %v %v", records, err)
			}
			partial := *records[0].Event.Ref
			// Crash after the final annotation but before partial cleanup.
			id := records[0].Event.Runner["transcriptCapture"]
			if _, err := b.AppendBatch(t.Context(), h, owner, []journal.Event{{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"transcriptCaptureComplete": id}}}); err != nil {
				t.Fatal(err)
			}
			if err := b.Retire(t.Context(), h, owner); err != nil {
				t.Fatal(err)
			}
			fault.armed.Store(true)
			fault.failRead.Store(fallback)
			h, owner, _, err = b.RecoverRun(t.Context(), journal.RunKey{Gaggle: "g", RunID: "run-mint"}, journal.RunWriterOptions{})
			fault.finish(t)
			if fallback {
				if !errors.Is(err, lease.ErrCommitUnknown) {
					t.Fatalf("want unresolved takeover: %v", err)
				}
				h, owner, _, err = b.RecoverRun(t.Context(), journal.RunKey{Gaggle: "g", RunID: "run-mint"}, journal.RunWriterOptions{})
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.ReadBlob(t.Context(), h, partial); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial cleanup: %v", err)
			}
			assertMintAppend(t, b, h, owner)
		})
	}
}

// A second lost outcome must retain both possible tokens until a fresh read
// selects one. A third attempted rotation cannot enlarge that uncertainty.
func TestPostgresRepeatedUnknownMintResolvesPredecessor(t *testing.T) {
	for _, rollback := range []bool{true, false} {
		name := "committed-B"
		if rollback {
			name = "rolled-back-B"
		}
		t.Run(name, func(t *testing.T) {
			b, config, fault := mintFaultBackend(t, false)
			fault.armed.Store(true)
			fault.failRead.Store(true)
			if _, _, err := b.CreateRun(t.Context(), mintRequest()); !errors.Is(err, lease.ErrCommitUnknown) {
				t.Fatal(err)
			}
			fault.finish(t)
			var firstToken string
			if err := config.Pool.QueryRow(t.Context(), "SELECT owner_token FROM journal.run_seq WHERE run_id='run-mint'").Scan(&firstToken); err != nil {
				t.Fatal(err)
			}
			fault.beforeCommit = rollback
			fault.fired.Store(false)
			fault.failRead.Store(true)
			if !rollback {
				if _, err := fault.admin.Exec(t.Context(), "ALTER SYSTEM SET synchronous_standby_names='journal_mint_absent_standby'"); err != nil {
					t.Fatal(err)
				}
				if _, err := fault.admin.Exec(t.Context(), "SELECT pg_reload_conf()"); err != nil {
					t.Fatal(err)
				}
				waitMintStandby(t, fault.admin)
			}
			key := journal.RunKey{Gaggle: "g", RunID: "run-mint"}
			if _, _, _, err := b.RecoverRun(t.Context(), key, journal.RunWriterOptions{}); !errors.Is(err, lease.ErrCommitUnknown) {
				t.Fatalf("second uncertainty: %v", err)
			}
			fault.finish(t)
			var currentToken string
			if err := config.Pool.QueryRow(t.Context(), "SELECT owner_token FROM journal.run_seq WHERE run_id='run-mint'").Scan(&currentToken); err != nil {
				t.Fatal(err)
			}
			if (currentToken == firstToken) != rollback {
				t.Fatal("fault did not produce the expected durable incarnation")
			}
			fault.fired.Store(false)
			fault.failRead.Store(true)
			if h, _, _, err := b.RecoverRun(t.Context(), key, journal.RunWriterOptions{}); !errors.Is(err, lease.ErrCommitUnknown) || h != "" {
				t.Fatalf("third uncertain rotation was not refused: %q %v", h, err)
			}
			if fault.fired.Load() {
				t.Fatal("issued a third COMMIT before resolving the pair")
			}
			fault.armed.Store(false)
			h, owner, _, err := b.RecoverRun(t.Context(), key, journal.RunWriterOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if string(owner) == currentToken {
				t.Fatal("recovery failed to rotate the selected incarnation")
			}
			assertMintAppend(t, b, h, owner)
		})
	}
}
