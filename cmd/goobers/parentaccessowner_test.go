package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type parentAccessFixture struct {
	service          *daemonCredentialService
	run              *journal.Run
	contract         childpod.Contract
	digest, token    string
	handler, missing http.Handler
}

func newParentAccessFixture(t *testing.T) parentAccessFixture {
	t.Helper()
	f := newPinnedChildFixture(t)
	run, env := configuredChildStage(t, f)
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	start := events[len(events)-1]
	q, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	instanceLog, _, err := journal.OpenInstanceLog(f.layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	s := newDaemonCredentialService(f.layout, f.cfg, nil, journal.NewRegistryScrubber(), instanceLog).withStageGrants(f.layout.Root, "127.0.0.1:8080", false)
	t.Cleanup(func() { unregisterDaemonStageGrants(f.layout.Root, s) })
	s.Replace(credentialPlaneDefinitionsFromSet(f.applied))
	if err = s.enableChildWorkflows(q, f.applied); err != nil {
		t.Fatal(err)
	}
	contract := childpod.Contract{Version: 1, Identity: id, ParentOrigin: env.ChildWorkflowOrigin, Stage: "plan", Attempt: 1, PodAttempt: int(start.Seq), StartedAt: start.Time, Ceiling: credentials.NewChildCeiling(false, env.Capabilities, env.Capabilities)}
	raw, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	digest := journal.Digest(raw)
	store := childpod.ParentBlobs{RunDir: run.Dir(), Identity: id}
	if err = store.Put(t.Context(), digest, raw); err != nil {
		t.Fatal(err)
	}
	if err = store.BindContract(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	token, err := s.grants.key.MintWorkflowParentPod(id.RunID, digest, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(s.grants.key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}

	if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1, Runner: map[string]any{"kind": childpod.ParentWriterStarted, "contractDigest": digest}}); err != nil {
		t.Fatal(err)
	}
	opts := []httpapi.HandlerOption{httpapi.WithAuthenticator(auth), httpapi.WithCredentialService(s)}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), append(opts, httpapi.WithWorkflowParentAccessService(parentAccessPlane{service: s}))...)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return parentAccessFixture{s, run, contract, digest, token, handler, missing}
}

func TestParentAccessHTTPRecoveryCustodyAndCleanup(t *testing.T) {
	f := newParentAccessFixture(t)
	path := "/api/v1/runs/" + f.contract.Identity.RunID + "/child-workflow-access"
	body, err := json.Marshal(map[string]string{"contractDigest": f.digest})
	if err != nil {
		t.Fatal(err)
	}
	request := func(h http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(f.missing, http.MethodPost, path, f.token, body); w.Code != http.StatusForbidden {
		t.Fatal("missing owner fallback", w.Code, w.Body)
	}
	out := request(f.handler, http.MethodPost, path, f.token, body)
	if out.Code != http.StatusOK || out.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("acquire", out.Code, out.Body)
	}
	var access httpapi.ChildWorkflowAccessResponse
	if err := json.Unmarshal(out.Body.Bytes(), &access); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(f.service.shared.Scrub([]byte(access.BearerToken)), []byte(access.BearerToken)) {
		t.Fatal("secret not registered")
	}
	if _, err := f.service.grants.key.VerifyChildWorkflowGrant(access.BearerToken); err != nil {
		t.Fatal(err)
	}
	again := request(f.handler, http.MethodPost, path, f.token, body)
	if again.Code != http.StatusOK || !bytes.Equal(out.Body.Bytes(), again.Body.Bytes()) {
		t.Fatal("lost delivery changed grant", again.Code, again.Body)
	}
	for _, bad := range [][]byte{[]byte(`{"contractDigest":"sha256:bad"}`), append(body[:len(body)-1:len(body)-1], []byte(`,"actor":"admin"}`)...)} {
		if w := request(f.handler, http.MethodPost, path, f.token, bad); w.Code != http.StatusBadRequest {
			t.Fatal("authored authority accepted", w.Code, w.Body)
		}
	}
	ordinary, err := f.service.grants.key.Mint(f.contract.Identity.RunID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if w := request(f.handler, http.MethodPost, path, ordinary, body); w.Code != http.StatusForbidden {
		t.Fatal("ordinary token exchanged", w.Code, w.Body)
	}
	// Exercise the shipped bounded client against the real signed HTTP owner.
	server := httptest.NewServer(f.handler)
	defer server.Close()
	client := childworkflow.ParentAccessClient{Endpoint: server.URL, Token: f.token, ContractDigest: f.digest}
	recovered, err := client.Acquire(t.Context(), f.contract.Identity.RunID)
	if err != nil || recovered.BearerToken != access.BearerToken {
		t.Fatal("client recovery", err)
	}
	if err := f.run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if w := request(f.handler, http.MethodPost, path, f.token, body); w.Code != http.StatusForbidden {
		t.Fatal("ended stage regained tools", w.Code, w.Body)
	}
	if err := client.Revoke(t.Context(), f.contract.Identity.RunID); err != nil {
		t.Fatal("late cleanup refused", err)
	}
	binding, err := f.service.childQueue.ChildAuthority(t.Context(), triggerqueue.ChildParent{Gaggle: f.contract.Identity.Gaggle, ParentRunID: f.contract.Identity.RunID}, f.contract.ParentOrigin.StageOccurrence)
	if err != nil || !binding.Revoked {
		t.Fatal("grant not revoked", err)
	}
}

func TestParentAccessHTTPRefusesJoinedWriter(t *testing.T) {
	f := newParentAccessFixture(t)
	if err := f.run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1, Runner: map[string]any{"kind": childpod.ParentWriterJoined, "contractDigest": f.digest}}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(f.handler)
	defer server.Close()
	client := childworkflow.ParentAccessClient{Endpoint: server.URL, Token: f.token, ContractDigest: f.digest}
	if access, err := client.Acquire(t.Context(), f.contract.Identity.RunID); err == nil || access != nil {
		t.Fatal("joined writer recovered tools")
	}
}
