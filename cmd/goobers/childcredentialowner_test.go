package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
)

func TestChildCredentialHTTPRequiresLiveExactAttemptThroughMaterialization(t *testing.T) {
	f := newChildKitFixture(t, true)
	s, id := f.writer.service, f.writer.identity
	launcher := &queuedChildLauncher{layout: s.layout, queue: s.childQueue, authority: s.children}
	s.childCredentials = launcher.credentialCeiling
	s.Replace(credentialPlaneDefinitionsFromSet(f.parent.applied))
	var err error
	s.log, _, err = journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.log.Close() })
	writer := f.writer.recorder.(*journal.Run)
	if err = writer.Append(journal.Event{Type: journal.EventStageStarted, Stage: "check", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(writer.Dir())
	if err != nil {
		t.Fatal(err)
	}
	event, err := childPodStarted(reader, "check", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	contract := childpod.Contract{Version: 1, Identity: id, Stage: "check", Attempt: 1, PodAttempt: int(event.Seq), StartedAt: event.Time, Ceiling: credentials.NewChildCeiling(false, []string{"agent:model"}, []string{"agent:model"})}
	raw, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	digest := journal.Digest(raw)
	blobs := childpod.ScopedBlobs{Queue: s.childQueue, Identity: f.child.Identity}
	if err = blobs.Put(t.Context(), digest, raw); err != nil {
		t.Fatal(err)
	}
	if err = blobs.BindContract(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	key, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := key.MintChildPod(id.RunID, digest, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	var duringResolve func()
	s.buildSources = func(credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error) {
		resolver, err := credentials.NewResolverWithExpiring(nil, nil, map[string]credentials.ResolveFunc{
			"agent:model": func(context.Context) (string, error) {
				resolved++
				if duringResolve != nil {
					duringResolve()
				}
				return "test-model-credential", nil
			},
		}, nil)
		return resolver, []credentials.Grant{{Capability: "agent:model", Ref: "agent:model"}}, err
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithCredentialService(s), httpapi.WithGeneratedChildCredentialService(childCredentialPlane{service: s}))
	if err != nil {
		t.Fatal(err)
	}
	request := func(input httpapi.CredentialResolveRequest) *httptest.ResponseRecorder {
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, apicontract.CredentialResolvePath, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		return out
	}
	input := httpapi.CredentialResolveRequest{RunID: id.RunID, Stage: "check", Attempt: 1}
	if out := request(input); out.Code != http.StatusForbidden || resolved != 0 {
		t.Fatal("unowned writer minted credentials", out.Code, out.Body)
	}
	custody := &childInvocationBlobs{ScopedBlobs: blobs, recorder: writer}
	retained := childpod.RetainedAttempt{Version: 1, Input: engine.ChildDispatchInput{Attempt: dispatcher.Attempt{RunID: id.RunID, Gaggle: id.Gaggle, Workflow: id.Workflow, Stage: "check", Number: 1, PodAttempt: contract.PodAttempt, ChildExecutionDigest: digest}, Queue: "worker", Eligible: []dispatcher.RunnerSpec{{OS: "linux", HostKind: instance.RunnerHostImage}}}}
	if err = custody.keepAttempt(t.Context(), retained); err != nil {
		t.Fatal(err)
	}
	if err = custody.BindContract(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []httpapi.CredentialResolveRequest{
		{RunID: strings.Repeat("b", 32), Stage: "check"},
		{RunID: id.RunID, Stage: "other"},
		{RunID: id.RunID, Stage: "check", Attempt: 2},
		{RunID: id.RunID, Stage: "check", Grant: true},
		{RunID: id.RunID, Stage: "check", Capabilities: []string{"repo:push"}},
	} {
		if out := request(invalid); out.Code != http.StatusForbidden || resolved != 0 {
			t.Fatal("invalid contract request minted credentials", invalid, out.Code, out.Body)
		}
	}
	if out := request(input); out.Code != http.StatusOK || resolved != 1 || !strings.Contains(out.Body.String(), "test-model-credential") || out.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("active model credential unavailable", out.Code, out.Body, resolved)
	}
	duringResolve = func() {
		if err := s.childQueue.FenceChildParent(t.Context(), f.child.Identity.ChildParent, "operator", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if out := request(input); out.Code != http.StatusForbidden || strings.Contains(out.Body.String(), "test-model-credential") || resolved != 2 {
		t.Fatal("cancellation during materialization leaked credential", out.Code, out.Body, resolved)
	}
	if out := request(input); out.Code != http.StatusForbidden || resolved != 2 {
		t.Fatal("cancelled child reached resolver", out.Code, out.Body, resolved)
	}
	// Cancellation revokes new effects, but the same unjoined writer must still
	// be able to return observational evidence through its bounded journal owner.
	testCancelledChildJournalCustody(t, s, auth, token, id, writer, blobs)
	testCancelledChildSurrenderCustody(t, s, auth, token, contract, digest, custody)
}

func testCancelledChildJournalCustody(t *testing.T, s *daemonCredentialService, auth httpapi.Authenticator, token string, id journal.RunIdentity, writer *journal.Run, blobs childpod.ScopedBlobs) {
	t.Helper()
	live, err := livejournal.NewWriter(func(gaggle string) (string, bool) { return s.layout.ForGaggle(gaggle).RunsDir(), gaggle == id.Gaggle })
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	release, err := live.Adopt(id.RunID, id.Gaggle, writer)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithJournalService(live), httpapi.WithGeneratedChildJournalService(childJournalPlane{writer: live, service: s}))
	if err != nil {
		t.Fatal(err)
	}
	request := func(ops ...livejournal.Op) *httptest.ResponseRecorder {
		data, err := json.Marshal(livejournal.EmitRequest{RunID: id.RunID, Gaggle: id.Gaggle, Ops: ops})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+id.RunID+"/journal/emit", bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		return out
	}
	data := []byte("final child observation")
	op := livejournal.Op{Kind: livejournal.OpArtifact, Key: "final-artifact", Artifact: &livejournal.ArtifactOp{Stage: "check", Attempt: 1, Name: "check/output", Data: data}}
	before := writer.Seq()
	for _, forged := range []journal.Event{
		{Type: journal.EventRunFinished, Stage: "check", Attempt: 1, Status: "completed"},
		{Type: journal.EventRunnerAnnotation, Stage: "check", Attempt: 1, Runner: map[string]any{"kind": childPodWriterJoined}},
	} {
		out := request(op, livejournal.Op{Kind: livejournal.OpAppend, Key: "forged", Event: &forged})
		if out.Code != http.StatusForbidden || writer.Seq() != before {
			t.Fatal("forged trailing operation partially applied", out.Code, out.Body)
		}
	}
	for range 2 {
		if out := request(op); out.Code != http.StatusOK || out.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("cancelled writer lost journal custody", out.Code, out.Body)
		}
	}
	if writer.Seq() != before+1 {
		t.Fatal("retry duplicated child artifact")
	}
	if got, err := blobs.Get(t.Context(), journal.Digest(data)); err != nil || !bytes.Equal(got, data) {
		t.Fatal("journal did not publish into child custody", string(got), err)
	}
}
