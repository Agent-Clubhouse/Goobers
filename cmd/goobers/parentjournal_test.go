package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
)

func TestParentJournalHTTPPreservesSignedBranchAndCustody(t *testing.T) {
	f := newPinnedChildFixture(t)
	run, _ := configuredChildStage(t, f)
	seq, origin, err := run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Stage: "plan", Attempt: 1, Branch: 2}, false)
	if err != nil {
		t.Fatal(err)
	}
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
	started := events[len(events)-1]
	c := childpod.Contract{Version: 1, Identity: id, ParentOrigin: origin, ParentBranch: 2, Stage: "plan", Attempt: 1, PodAttempt: int(seq), StartedAt: started.Time, Ceiling: credentials.NewChildCeiling(false, nil, nil)}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	digest := journal.Digest(raw)
	store := childpod.ParentBlobs{RunDir: run.Dir(), Identity: id}
	if err := store.Put(t.Context(), digest, raw); err != nil {
		t.Fatal(err)
	}
	if err := store.BindContract(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	mark := func(kind string) {
		t.Helper()
		if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1, Branch: 2, Runner: map[string]any{"kind": kind, "contractDigest": digest}}); err != nil {
			t.Fatal(err)
		}
	}
	mark(childpod.ParentWriterStarted)
	keys, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(keys, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := keys.MintWorkflowParentPod(id.RunID, digest, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service := &daemonCredentialService{layout: f.layout}
	live, err := livejournal.NewWriter(func(gaggle string) (string, bool) { return f.layout.ForGaggle(gaggle).RunsDir(), gaggle == id.Gaggle })
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	release, err := live.Adopt(id.RunID, id.Gaggle, run)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	opts := []httpapi.HandlerOption{httpapi.WithAuthenticator(auth), httpapi.WithJournalService(live)}
	makeHandler := func() http.Handler {
		t.Helper()
		h, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	handler := makeHandler()
	send := func(ops ...livejournal.Op) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(livejournal.EmitRequest{RunID: id.RunID, Gaggle: id.Gaggle, Ops: ops})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+id.RunID+"/journal/emit", bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	artifact := livejournal.Op{Kind: livejournal.OpArtifact, Key: "artifact", Artifact: &livejournal.ArtifactOp{Stage: "plan", Attempt: 1, Name: "plan/output", Data: []byte("observed")}}
	before := run.Seq()
	if w := send(artifact); w.Code != http.StatusForbidden || run.Seq() != before {
		t.Fatal("parent inherited ordinary writer", w.Code, w.Body)
	}
	opts = append(opts, httpapi.WithWorkflowParentJournalService(parentJournalPlane{writer: live, service: service}))
	handler = makeHandler()
	for _, forged := range []journal.Event{
		{Type: journal.EventRunFinished, Stage: "plan", Attempt: 1, Status: "completed"},
		{Type: journal.EventAgentMessage, Stage: "plan", Attempt: 1, Branch: 1},
		{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1, Runner: map[string]any{"kind": childpod.ParentWriterJoined}},
	} {
		if w := send(artifact, livejournal.Op{Kind: livejournal.OpAppend, Key: "forged", Event: &forged}); w.Code != http.StatusForbidden || run.Seq() != before {
			t.Fatal("forged batch partially committed", w.Code, w.Body)
		}
	}
	assertOK := func(ops ...livejournal.Op) {
		t.Helper()
		if w := send(ops...); w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("parent observation failed", w.Code, w.Body)
		}
	}
	assertOK(artifact)
	assertOK(artifact)
	if run.Seq() != before+1 {
		t.Fatal("retry duplicated artifact")
	}
	data := []byte("parent transcript\n")
	ref, err := journal.SpanRef(data)
	if err != nil {
		t.Fatal(err)
	}
	scoped := childpod.ParentAttemptBlobs{Store: store, ContractDigest: digest}
	if err := scoped.Put(t.Context(), ref.Digest, data); err != nil {
		t.Fatal(err)
	}
	assertOK(livejournal.Op{Kind: livejournal.OpSpan, Key: "span", Span: &livejournal.SpanOp{Stage: "plan", Attempt: 1, Name: "span", Ref: ref}})
	artifactRef, err := journal.ArtifactRef([]byte("adopted"))
	if err != nil {
		t.Fatal(err)
	}
	if err := scoped.Put(t.Context(), artifactRef.Digest, []byte("adopted")); err != nil {
		t.Fatal(err)
	}
	assertOK(livejournal.Op{Kind: livejournal.OpArtifact, Key: "adopted", Artifact: &livejournal.ArtifactOp{Stage: "plan", Attempt: 1, Name: "plan/adopted", Ref: &artifactRef}})
	capture := strings.Repeat("a", 32)
	checkpoint := func(action, key string, cp livejournal.TranscriptCheckpointOp) {
		t.Helper()
		cp.Capture, cp.Action, cp.Stage, cp.Name = capture, action, "plan", "copilot-cli.transcript"
		assertOK(livejournal.Op{Kind: livejournal.OpTranscriptCheckpoint, Key: capture + "/" + key, Checkpoint: &cp})
	}
	checkpoint("open", "open", livejournal.TranscriptCheckpointOp{})
	checkpoint("append", "checkpoint/0", livejournal.TranscriptCheckpointOp{Stream: "process-output/1", Data: data, Reason: "checkpoint"})
	checkpoint("final", "final", livejournal.TranscriptCheckpointOp{FinalRef: &ref})
	assertOK(livejournal.Op{Kind: livejournal.OpSpan, Key: "adopt-transcript", Span: &livejournal.SpanOp{Stage: "plan", Attempt: 1, Name: "copilot-cli.transcript", Ref: ref}})
	missing, err := journal.SpanRef([]byte("unavailable span"))
	if err != nil {
		t.Fatal(err)
	}
	assertOK(livejournal.Op{Kind: livejournal.OpSpan, Key: "missing-span", Span: &livejournal.SpanOp{Stage: "plan", Attempt: 1, Name: "missing", Ref: missing}})
	heartbeat := livejournal.Op{Kind: livejournal.OpAppend, Key: "heartbeat", Event: &journal.Event{Type: journal.EventStageHeartbeat, Stage: "plan", Attempt: 1}}
	assertOK(heartbeat)
	events, err = reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[journal.EventType]bool{}
	finals, adopted := 0, 0
	for _, e := range events {
		if e.Seq <= before {
			continue
		}
		if e.Branch != 2 {
			t.Fatalf("parent observation escaped branch: %+v", e)
		}
		kinds[e.Type] = true
		if e.Type == journal.EventSpanRecorded && e.Name == "copilot-cli.transcript" {
			finals++
		}
		if e.Runner["transcriptCaptureAdopted"] != nil {
			adopted++
		}
	}
	if finals != 1 || adopted != 1 {
		t.Fatalf("transcript adoption duplicated/lost canonical evidence: finals=%d adopted=%d", finals, adopted)
	}
	if !kinds[journal.EventError] || !kinds[journal.EventArtifactRecorded] || !kinds[journal.EventSpanRecorded] || !kinds[journal.EventStageHeartbeat] {
		t.Fatal("missing branch evidence", kinds)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1, Branch: 2}); err != nil {
		t.Fatal(err)
	}
	heartbeat.Key = "late-heartbeat"
	if w := send(heartbeat); w.Code != http.StatusForbidden {
		t.Fatal("finished parent extended heartbeat", w.Code, w.Body)
	}
	artifact.Key = "late-output"
	assertOK(artifact)
	mark(childpod.ParentWriterJoined)
	artifact.Key = "joined-output"
	if w := send(artifact); w.Code != http.StatusForbidden {
		t.Fatal("joined parent retained writer", w.Code, w.Body)
	}
}
