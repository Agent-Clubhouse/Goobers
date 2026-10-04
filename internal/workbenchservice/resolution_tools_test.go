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

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/sessionops"
	"github.com/goobers/goobers/internal/workbench"
)

func TestNeedsHumanToolHTTPKeepsActualSessionAssessmentAndReceipt(t *testing.T) {
	resolver, transport, _ := resolutionFixture(t, "github")
	runs := t.TempDir()
	id := resolver.identity
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
	access, closeAccess, err := bridge.Open(sessionops.Invocation{Identity: id, Actor: resolver.actor, Lease: resolver.lease, StageSequence: run.Seq(), Attempt: 1, Resolver: resolver, ResolveBindings: []string{"items"}, Recorder: run})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAccess()
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(httpapi.DenyAllAuthenticator{}), httpapi.WithSessionOperations(bridge))
	if err != nil {
		t.Fatal(err)
	}
	call := func(operation string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, strings.ReplaceAll(sessioning.OperationPath, "{run}", id.RunID)+"/"+operation, bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+access.BearerToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	inspected := call("inspect_needs_human", sessioning.BacklogReadRequest{SourceBindingID: "items", BacklogItemRequest: workbench.BacklogItemRequest{ID: "42", ExpectedSourceID: "987654"}})
	var observation workbench.NeedsHumanObservation
	if err = json.Unmarshal(inspected.Body.Bytes(), &observation); err != nil || inspected.Code != 200 {
		t.Fatal(inspected.Code, inspected.Body.String(), err)
	}
	request := sessioning.NeedsHumanResolutionRequest{SourceBindingID: "items", RequestID: "assessed-one", NeedsHumanResolutionRequest: assessedResolution(observation)}
	response := call("resolve_needs_human", request)
	var command workbench.NeedsHumanResolutionCommand
	if err = json.Unmarshal(response.Body.Bytes(), &command); err != nil || response.Code != 200 || command.State != "confirmed" {
		t.Fatal(response.Code, response.Body.String(), err)
	}
	replay := call("resolve_needs_human", request)
	if replay.Code != 200 || transport.mutations != 1 {
		t.Fatal(replay.Code, replay.Body.String(), transport.mutations)
	}
	receipt := call("get_needs_human_receipt", sessioning.BacklogReceiptRequest{SourceBindingID: "items", CommandID: command.ID})
	if receipt.Code != 200 {
		t.Fatal(receipt.Code, receipt.Body.String())
	}
	reader, err := journal.OpenReadOnly(filepath.Join(runs, id.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.EventsBounded(8<<20, 8192)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, event := range events {
		if event.Runner["commandId"] == command.ID {
			found++
			if event.Runner["humanIssuer"] != resolver.actor.Issuer || event.Runner["humanSubject"] != resolver.actor.Subject || len(event.Artifacts) != 1 {
				t.Fatal("missing actual human receipt", event)
			}
			raw, err := reader.ArtifactBytesBounded(event.Artifacts[0], 2<<20)
			if err != nil {
				t.Fatal(err)
			}
			var evidence workbench.NeedsHumanResolutionCommand
			if err = json.Unmarshal(raw, &evidence); err != nil || evidence.Assessment.Rationale != request.Rationale || evidence.Origin.RunID != id.RunID || evidence.Origin.MessageID != id.Session.MessageID {
				t.Fatal("missing assessment origin", evidence, err)
			}
		}
	}
	if found != 3 {
		t.Fatal("missing durable audit", found)
	}
	closeAccess()
	if denied := call("get_needs_human_receipt", sessioning.BacklogReceiptRequest{SourceBindingID: "items", CommandID: command.ID}); denied.Code != 401 {
		t.Fatal(denied.Code)
	}
}
