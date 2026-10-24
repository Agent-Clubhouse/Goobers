package podauth

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

type terminalSeam struct{ calls int }

func (s *terminalSeam) Emit(context.Context, livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	return livejournal.EmitResponse{}, nil
}
func (s *terminalSeam) CommitTerminalIntervention(context.Context, livejournal.TerminalInterventionRequest) (journal.TerminalInterventionReceipt, error) {
	s.calls++
	return journal.TerminalInterventionReceipt{Seq: 4, Segment: 2, Resumed: true}, nil
}

func TestTerminalSeamRefusesPodAndOtherWorkerBearers(t *testing.T) {
	signer := testSignedKey(t)
	mint := func(token string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	tokens := map[string]string{
		"terminal":  mint(signer.MintWorkerTerminalIntervention("worker", time.Minute)),
		"pod":       mint(signer.MintScoped("run-1", time.Minute, ScopeJournal)),
		"config":    mint(signer.MintWorkerConfigDigest("worker", time.Minute)),
		"child":     mint(signer.MintWorkerChildAdmission("worker", time.Minute)),
		"blob":      mint(signer.MintWorkerBlob("worker", time.Minute)),
		"surrender": mint(signer.MintWorkerSurrender("worker", time.Minute)),
	}
	// The handler itself also enforces the issuer under permissive middleware.
	for _, authorizer := range []httpapi.Authorizer{httpapi.RequireRoles(), httpapi.AllowAll} {
		for name, token := range tokens {
			t.Run(name, func(t *testing.T) {
				service := &terminalSeam{}
				handler, err := httpapi.NewHandler(checkpointUnusedReader{}, authorizer, log.New(io.Discard, "", 0), httpapi.WithAuthenticator(newTestAuthenticator(t, signer)), httpapi.WithJournalService(service))
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/run-1/journal/terminal-intervention", strings.NewReader(`{"runId":"run-1","gaggle":"web"}`))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer "+token)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				want, calls := http.StatusForbidden, 0
				if name == "terminal" {
					want, calls = http.StatusOK, 1
				}
				if response.Code != want || service.calls != calls {
					t.Fatalf("status=%d calls=%d body=%s", response.Code, service.calls, response.Body)
				}
			})
		}
	}
}
