package livejournal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type terminalMinter struct {
	stubMinter
	worker string
}

func (m *terminalMinter) MintWorkerTerminalIntervention(worker string, _ time.Duration) (string, error) {
	m.worker = worker
	return "dedicated-terminal-token", nil
}

func TestTerminalClientUsesOnlyDedicatedCredential(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("Authorization"); got != "Bearer dedicated-terminal-token" {
			t.Errorf("credential=%q", got)
		}
		if r.URL.Path != "/api/v1/runs/run-1/journal/terminal-intervention" {
			t.Errorf("path=%s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(journal.TerminalInterventionReceipt{Seq: 4, Segment: 2, Resumed: true})
	}))
	defer server.Close()
	minter := &terminalMinter{}
	emitter := &HTTPEmitter{BaseURL: server.URL, Token: "pod-bearer", Minter: minter, TerminalWorkerID: "worker-1"}
	req := liveTerminalRequest("run-1", 2, 1)
	if _, err := emitter.CommitTerminalIntervention(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if minter.worker != "worker-1" || calls != 1 {
		t.Fatalf("worker=%s calls=%d", minter.worker, calls)
	}
	emitter.Minter = &stubMinter{token: "pod-token"}
	if _, err := emitter.CommitTerminalIntervention(t.Context(), req); !errors.Is(err, ErrTerminalAuthority) {
		t.Fatal("pod-only minter acquired terminal authority")
	}
	emitter.Minter = minter
	emitter.TerminalWorkerID = ""
	if _, err := emitter.CommitTerminalIntervention(t.Context(), req); !errors.Is(err, ErrTerminalAuthority) {
		t.Fatal("pod emitter acquired worker authority")
	}
	if calls != 1 {
		t.Fatalf("unprivileged request reached transport: %d", calls)
	}
}

func TestTerminalClientClassifiesRefusalsAndNeverRetriesOrRedirects(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{{400, journal.ErrInterventionInvalid}, {401, ErrTerminalAuthority}, {403, ErrTerminalAuthority}, {409, journal.ErrInterventionConflict}, {500, nil}, {307, nil}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/unexpected")
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			emitter := &HTTPEmitter{BaseURL: server.URL, Minter: &terminalMinter{}, TerminalWorkerID: "worker"}
			_, err := emitter.CommitTerminalIntervention(context.Background(), liveTerminalRequest("run-1", 2, 1))
			if err == nil || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
			if tc.want == nil && (errors.Is(err, journal.ErrInterventionInvalid) || errors.Is(err, journal.ErrInterventionConflict) || errors.Is(err, ErrTerminalAuthority)) {
				t.Fatalf("transport failure became definitive: %v", err)
			}
		})
	}
}
