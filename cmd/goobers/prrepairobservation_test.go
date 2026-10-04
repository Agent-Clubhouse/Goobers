package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbenchservice"
)

func TestWorkbenchHostChecksRetainedRepairAfterSessionClosure(t *testing.T) {
	u, source, input := repairHostFixture(t)
	q, now := u.durableTriggers.queue, time.Now()
	record, _, err := q.AcceptPRRepairCommand(t.Context(), input, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := q.ClaimPRRepairCommand(t.Context(), input.Scope, record.ID, record.RequestDigest, now); err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	original := sessioning.PRRepairReceipt{OperationDigest: input.OperationDigest, Outcome: "unknown", MutationAttempted: true}
	if _, err = q.CompletePRRepairCommand(t.Context(), input.Scope, record.ID, record.RequestDigest, original, now); err != nil {
		t.Fatal(err)
	}
	source.Lease.Close()
	if err = q.CompleteSessionTurn(t.Context(), source.Identity.Session.AcceptanceID, triggerqueue.SessionCompletion{RunID: source.Identity.RunID, Outcome: "success", Text: "Repair response lost"}, now); err != nil {
		t.Fatal(err)
	}
	closeCommand := triggerqueue.SessionCommand{Gaggle: input.Scope.Gaggle, Actor: input.Scope.Actor, RequestID: "close", RequestDigest: sessioning.Digest([]byte("close"))}
	if _, err = q.CloseSession(t.Context(), closeCommand, input.Origin.SessionID, "done", now); err != nil {
		t.Fatal(err)
	}
	u.configureWorkbenchReads()
	principal := httpapi.Principal{Issuer: source.Actor.Issuer, Subject: source.Actor.Subject, Roles: []httpapi.Role{httpapi.RoleOperate}}
	request := func(method, suffix string) sessioning.PRRepairCommandView {
		t.Helper()
		opts := append(u.apiHandlerOpts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &principal}))
		handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
		if err != nil {
			t.Fatal(err)
		}
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader("{}")
		}
		req := httptest.NewRequest(method, "/api/v1/gaggles/gaggle/pr-repairs/"+record.ID+suffix, body)
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		var view sessioning.PRRepairCommandView
		if out.Code != http.StatusOK || json.Unmarshal(out.Body.Bytes(), &view) != nil {
			t.Fatal(out.Code, out.Body.String())
		}
		return view
	}
	if view := request(http.MethodGet, ""); view.State != "unknown" || view.Actor != source.Actor {
		t.Fatal(view)
	}
	client := &hostRepairClient{target: *input.Target, apply: func(context.Context) error { t.Fatal("observation retried a repair"); return nil }}
	client.target.HeadSHA = strings.Repeat("c", 40)
	credentials := 0
	u.installPRRepairRecovery(func(_ context.Context, binding workbenchservice.ReadBinding, credential interactiveaccess.Credential) (workbenchservice.PRRepairClient, error) {
		if credential.Value != "host-human-token" || binding.Source.Spec.Name != input.Scope.SourceBindingID {
			t.Fatal("wrong observation source or credential")
		}
		credentials++
		return client, nil
	})
	view := request(http.MethodPost, "/check")
	if view.State != "observed-applied" || view.Receipt == nil || *view.Receipt != original || len(view.Observations) != 1 || view.Observations[0].Checker != source.Actor || credentials != 1 || client.effects != 0 {
		t.Fatal(view, credentials, client.effects)
	}
	if view = request(http.MethodGet, ""); view.State != "observed-applied" || credentials != 1 {
		t.Fatal(view, credentials)
	}
}
