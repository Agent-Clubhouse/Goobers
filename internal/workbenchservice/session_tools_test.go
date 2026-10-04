package workbenchservice

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/sessionops"
	"github.com/goobers/goobers/internal/workbench"
)

func TestSessionNativeToolHTTPUsesDurableNativeProviderCustody(t *testing.T) {
	for _, provider := range []apiv1.Provider{"github", "ado"} {
		t.Run(string(provider), func(t *testing.T) {
			service, transport, g, p, request := writerFixture(t, provider)
			lease, err := service.ReadService.Permissions.BeginSessionExecution(t.Context(), p, g.Name)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			actor := sessioning.Actor{Issuer: p.Issuer, Subject: p.Subject}
			writer, err := service.ForSession(&g, lease, actor)
			if err != nil {
				t.Fatal(err)
			}
			id := sessionToolIdentity(g.Name)
			runs := t.TempDir()
			run, err := journal.Create(runs, id, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = run.Close() }()
			if err = run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "respond", Attempt: 1}); err != nil {
				t.Fatal(err)
			}
			registry := journal.NewRegistryScrubber()
			registry.Register([]byte("human-read-canary"))
			bridge := &sessionops.Bridge{Endpoint: "https://daemon.invalid", Scrubber: registry, Secrets: registry, Now: time.Now}
			access, closeAccess, err := bridge.Open(sessionops.Invocation{Identity: id, Actor: actor, Lease: lease, StageSequence: run.Seq(), Attempt: 1, Writer: writer, WriteBindings: []string{"items"}, Recorder: run})
			if err != nil {
				t.Fatal(err)
			}
			defer closeAccess()
			handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(httpapi.DenyAllAuthenticator{}), httpapi.WithSessionOperations(bridge))
			if err != nil {
				t.Fatal(err)
			}
			command := sessioning.BacklogEditRequest{SourceBindingID: "items", RequestID: "edit-1", BacklogPatchRequest: request}
			call := func(operation string, body any) *httptest.ResponseRecorder {
				t.Helper()
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest(http.MethodPost, strings.ReplaceAll(sessioning.OperationPath, "{run}", id.RunID)+"/"+operation, bytes.NewReader(raw))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+access.BearerToken)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, r)
				return response
			}
			first := call("edit_backlog_item", command)
			var result workbench.BacklogEditCommand
			if err = json.Unmarshal(first.Body.Bytes(), &result); err != nil || first.Code != 200 || result.State != "confirmed" {
				t.Fatal(first.Code, first.Body.String(), err)
			}
			replay := call("edit_backlog_item", command)
			var duplicate workbench.BacklogEditCommand
			if err = json.Unmarshal(replay.Body.Bytes(), &duplicate); err != nil || replay.Code != 200 || !duplicate.Duplicate || duplicate.ID != result.ID || transport.patches != 1 {
				t.Fatal(replay.Code, replay.Body.String(), transport.patches, err)
			}
			receipt := call("get_backlog_edit_receipt", sessioning.BacklogReceiptRequest{SourceBindingID: "items", CommandID: result.ID})
			if receipt.Code != 200 || transport.patches != 1 {
				t.Fatal(receipt.Code, receipt.Body.String())
			}
			changed := command
			changed.Field = "labels"
			changed.Value = nil
			changed.Values = []string{"needs-human"}
			if denied := call("edit_backlog_item", changed); denied.Code == 200 || transport.patches != 1 {
				t.Fatal("control-label edit escaped", denied.Code)
			}
			rd, err := journal.OpenReadOnly(filepath.Join(runs, id.RunID))
			if err != nil {
				t.Fatal(err)
			}
			events, err := rd.EventsBounded(8<<20, 8192)
			if err != nil {
				t.Fatal(err)
			}
			found := 0
			for _, event := range events {
				if event.Runner["commandId"] == result.ID {
					found++
					if event.Runner["humanIssuer"] != actor.Issuer || event.Runner["humanSubject"] != actor.Subject || len(event.Artifacts) != 1 {
						t.Fatal("missing human receipt evidence", event)
					}
				}
			}
			if found != 3 {
				t.Fatal("missing actual command evidence", found)
			}
			closeAccess()
			if denied := call("get_backlog_edit_receipt", sessioning.BacklogReceiptRequest{SourceBindingID: "items", CommandID: result.ID}); denied.Code != 401 {
				t.Fatal("grant outlived invocation", denied.Code)
			}
		})
	}
}
func sessionToolIdentity(gaggle string) journal.RunIdentity {
	run := strings.Repeat("a", 32)
	digest := journal.Digest([]byte("pinned session"))
	return journal.RunIdentity{InstanceID: "instance", RunID: run, Gaggle: gaggle, Workflow: "interactive-session", ConfigGeneration: digest, WorkflowDigest: digest, GooberDigest: digest, Trigger: journal.Trigger{Kind: journal.TriggerSignal, Ref: "session:session-one:turn-one"}, Session: &journal.SessionLineage{Gaggle: gaggle, SessionID: "session-one", TurnID: "turn-one", MessageID: "message-one", AcceptanceID: "trigger-" + run, EnvelopeDigest: digest, InputDigest: digest}}
}
