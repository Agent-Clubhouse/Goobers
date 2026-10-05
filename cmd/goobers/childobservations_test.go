package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
)

func TestChildAttemptHTTPJournalAndSurrenderUseSignedStage(t *testing.T) {
	f := actualChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	digest, run := publishChildPodContract(t, f, id, "check", 1)
	t.Cleanup(func() { _ = run.Close() })
	service := &daemonCredentialService{layout: f.launcher.layout, childQueue: f.service.queue}
	key, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := key.MintChildPod(id.RunID, digest, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return service.layout.ForGaggle(id.Gaggle).RunsDir(), true })
	if err != nil {
		t.Fatal(err)
	}
	release, err := writer.Adopt(id.RunID, id.Gaggle, run)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(); writer.Close() })
	surrender, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithJournalService(containedJournalPlane{JournalService: writer, service: service}), httpapi.WithSurrenderService(containedSurrenderPlane{SurrenderDir: surrender, service: service}))
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out
	}
	emit := livejournal.EmitRequest{RunID: id.RunID, Gaggle: id.Gaggle, Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: "annotation", Time: time.Now(), Event: &journal.Event{Type: journal.EventRunnerAnnotation, Stage: id.RunID + ":check", Runner: map[string]any{"kind": "agent-telemetry-fidelity"}}}}}
	path := "/api/v1/runs/" + id.RunID + "/journal/emit"
	if out := post(path, emit); out.Code != 200 {
		t.Fatalf("own observation %d %s", out.Code, out.Body)
	}
	emit.Ops[0].Event.Stage = "sibling"
	if out := post(path, emit); out.Code == 200 {
		t.Fatal("sibling journal accepted")
	}
	blobs := childpod.ChildAttemptBlobs{Store: childpod.ScopedBlobs{Queue: f.service.queue, Identity: f.submission.Child.Identity}, ContractDigest: digest}
	contractRaw, err := blobs.Get(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := childpod.DecodeContract(contractRaw, digest)
	if err != nil {
		t.Fatal(err)
	}
	output, _ := json.Marshal(childpod.Output{Version: 1, ContractDigest: digest})
	outputDigest := journal.Digest(output)
	if err = blobs.Put(t.Context(), outputDigest, output); err != nil {
		t.Fatal(err)
	}
	result := dispatcher.SurrenderedResult{RecoveryAcknowledged: true, ChildWorkspaceDigest: outputDigest, Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}
	path = fmt.Sprintf("/api/v1/runs/%s/stages/check/attempts/%d/surrender", id.RunID, contract.PodAttempt)
	if out := post(path, result); out.Code != 200 {
		t.Fatalf("own surrender %d %s", out.Code, out.Body)
	}
	if out := post(strings.Replace(path, "/check/", "/sibling/", 1), result); out.Code == 200 {
		t.Fatal("sibling surrender accepted")
	}
	if err = run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "check", Attempt: 1, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	if out := post(path, result); out.Code == 200 {
		t.Fatal("finished stage surrendered without pending host custody")
	}
}
