package podauth

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func controllerBatch(key string) livejournal.EmitRequest {
	return livejournal.EmitRequest{RunID: "controller-run", Gaggle: "web", Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: key, Event: &journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1}}}}
}

func controllerToken(t *testing.T, key *SignedKey, batch livejournal.EmitRequest) string {
	t.Helper()
	digest, err := livejournal.ControllerJournalDigest(batch)
	if err != nil {
		t.Fatal(err)
	}
	token, err := key.MintControllerJournal(batch.RunID, digest, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestControllerJournalCredentialDomainExpiryAndProofBinding(t *testing.T) {
	now := time.Now()
	key := grantKey(t, 7, &now)
	batch := controllerBatch("controller-start")
	token := controllerToken(t, key, batch)
	if run, _, err := key.VerifyControllerJournal(token); err != nil || run != batch.RunID {
		t.Fatalf("valid controller grant: %q %v", run, err)
	}
	pod, _ := key.Mint(batch.RunID, time.Minute)
	worker, _ := key.MintWorkerConfigDigest("worker", time.Minute)
	blob, _ := key.MintWorkerBlob("worker", time.Minute)
	launch := launchToken(t, key, launchFixture())
	credential, _, err := key.MintCredentialGrant(testCredentialGrant(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{pod, worker, blob, credential, launch, token + "altered", strings.Repeat("x", 2049)} {
		if _, _, err := key.VerifyControllerJournal(other); err == nil {
			t.Fatal("unrelated credential admitted")
		}
		_, suffix, _ := strings.Cut(other, ".")
		if _, _, err := key.VerifyControllerJournal(livejournal.ControllerJournalTokenPrefix + suffix); err == nil {
			t.Fatal("MAC domain substitution admitted")
		}
	}
	if _, err := key.VerifyLaunchGrant(token); err == nil {
		t.Fatal("controller grant gained launch authority")
	}
	if _, _, err := key.verifySigned(token); err == nil {
		t.Fatal("controller grant gained pod authority")
	}
	otherKey := grantKey(t, 9, &now)
	if _, _, err := otherKey.VerifyControllerJournal(token); err == nil {
		t.Fatal("wrong signer admitted")
	}
	e := *batch.Ops[0].Event
	e.Seq = 2
	proof := key.SealControllerStart(batch.RunID, batch.Ops[0].Key, e)
	if !key.VerifyControllerStart(batch.RunID, batch.Ops[0].Key, e, proof) {
		t.Fatal("valid durable proof refused")
	}
	for _, mutate := range []func(*journal.Event){func(e *journal.Event) { e.Seq++ }, func(e *journal.Event) { e.Stage = "review" }, func(e *journal.Event) { e.Branch++ }, func(e *journal.Event) { e.Attempt++ }, func(e *journal.Event) { e.AttemptClass = journal.AttemptInfra }, func(e *journal.Event) { e.Type = journal.EventReviewerStarted }} {
		changed := e
		mutate(&changed)
		if key.VerifyControllerStart(batch.RunID, batch.Ops[0].Key, changed, proof) {
			t.Fatal("altered anchor admitted")
		}
	}
	if key.VerifyControllerStart("another-run", batch.Ops[0].Key, e, proof) || key.VerifyControllerStart(batch.RunID, "another-key", e, proof) {
		t.Fatal("copied origin admitted")
	}
	now = now.Add(2 * time.Minute)
	if _, _, err := key.VerifyControllerJournal(token); err == nil {
		t.Fatal("expired credential admitted")
	}
	if !key.VerifyControllerStart(batch.RunID, batch.Ops[0].Key, e, proof) {
		t.Fatal("durable origin expired with transport")
	}
}

func TestControllerJournalOriginSurvivesRestartAndRejectsPodMarkers(t *testing.T) {
	now := time.Now()
	key := grantKey(t, 7, &now)
	runs := filepath.Join(t.TempDir(), "runs")
	newWriter := func() *livejournal.Writer {
		w, err := livejournal.NewWriter(func(string) (string, bool) { return runs, true }, livejournal.WithControllerStartAuthority(key))
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	w := newWriter()
	defer func() { w.Close() }()
	auth, err := NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	newHandler := func() http.Handler {
		h, err := httpapi.NewHandler(launchUnusedReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithJournalService(w), httpapi.WithControllerJournalVerifier(key))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := newHandler()
	emit := func(token string, batch livejournal.EmitRequest) livejournal.EmitResponse {
		t.Helper()
		raw, err := json.Marshal(batch)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+batch.RunID+"/journal/emit", bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, r)
		if recorder.Code != http.StatusOK {
			t.Fatalf("emit status %d: %s", recorder.Code, recorder.Body)
		}
		var response livejournal.EmitResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	batch := controllerBatch("controller-start")
	batch.Open = &livejournal.OpenHeader{Identity: journal.RunIdentity{RunID: batch.RunID, Gaggle: "web", Workflow: "wf", WorkflowVersion: 1, WorkflowDigest: journal.Digest([]byte("workflow")), Trigger: journal.Trigger{Kind: journal.TriggerManual}}, Graph: []byte(`{"nodes":[]}`), Definition: []byte(`{"name":"wf"}`)}
	batch.Ops = append([]livejournal.Op{{Kind: livejournal.OpAppend, Key: "run-start", Event: &journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning)}}}, batch.Ops...)
	first := emit(controllerToken(t, key, batch), batch)
	if len(first.Starts) != 1 {
		t.Fatalf("trusted start absent: %+v", first)
	}
	reader, err := journal.OpenRead(filepath.Join(runs, batch.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	proof := events[len(events)-1].Runner[livejournal.ControllerStartProofField]
	pod, _ := key.MintScoped(batch.RunID, time.Minute, ScopeJournal)
	untrusted := controllerBatch("pod-start")
	untrusted.Ops[0].Event.Runner = map[string]any{livejournal.ControllerStartProofField: proof}
	if got := emit(pod, untrusted); len(got.Starts) != 0 {
		t.Fatal("pod-supplied marker became trusted")
	}
	// Even a subsequent legitimate emission cannot promote a pod-owned key.
	if got := emit(controllerToken(t, key, untrusted), untrusted); len(got.Starts) != 0 {
		t.Fatal("dedup promoted an untrusted start")
	}
	w.Close()
	w = newWriter()
	h = newHandler()
	if got := emit(pod, untrusted); len(got.Starts) != 0 {
		t.Fatal("restart promoted a pod start")
	}
	if got := emit(controllerToken(t, key, batch), batch); !reflect.DeepEqual(got.Starts, first.Starts) {
		t.Fatalf("trusted dedup changed on restart: %+v / %+v", got, first)
	}
	// A later genuine reviewer start receives its own exact anchor despite the pod event.
	review := controllerBatch("controller-review")
	review.Ops[0].Event.Type = journal.EventReviewerStarted
	review.Ops[0].Event.Stage = "review"
	if got := emit(controllerToken(t, key, review), review); len(got.Starts) != 1 || got.Starts[0].Seq != first.Starts[0].Seq+2 {
		t.Fatalf("interleaved reviewer anchor: %+v", got)
	}
	reader, err = journal.OpenRead(filepath.Join(runs, batch.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err = reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Runner[livejournal.EmitKeyRunnerField] == "pod-start" && e.Runner[livejournal.ControllerStartProofField] != nil {
			t.Fatal("caller proof persisted")
		}
	}
	server := httptest.NewServer(h)
	defer server.Close()
	client := livejournal.HTTPEmitter{BaseURL: server.URL, ControllerMinter: key}
	genuine := controllerBatch("via-controller-client")
	if got, err := client.Emit(t.Context(), genuine); err != nil || len(got.Starts) != 1 {
		t.Fatalf("controller transport: %+v %v", got, err)
	}
}

func TestControllerJournalHTTPExactBodyAndRunConfinement(t *testing.T) {
	now := time.Now()
	key := grantKey(t, 7, &now)
	auth, err := NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := livejournal.NewWriter(func(string) (string, bool) { return t.TempDir(), true }, livejournal.WithControllerStartAuthority(key))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	h, err := httpapi.NewHandler(launchUnusedReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithJournalService(w), httpapi.WithControllerJournalVerifier(key))
	if err != nil {
		t.Fatal(err)
	}
	batch := controllerBatch("controller-start")
	token := controllerToken(t, key, batch)
	for _, change := range []string{"body", "run", "route", "method"} {
		t.Run(change, func(t *testing.T) {
			altered := controllerBatch("controller-start")
			path := "/api/v1/runs/" + batch.RunID + "/journal/emit"
			method := http.MethodPost
			switch change {
			case "body":
				altered.Ops[0].Event.Attempt++
			case "run":
				altered.RunID = "another-run"
				path = "/api/v1/runs/another-run/journal/emit"
			case "route":
				path = apicontract.LaunchReceiptPath
			case "method":
				method = http.MethodGet
				path = apicontract.ConfigDigestPath
			}
			raw, _ := json.Marshal(altered)
			r := httptest.NewRequest(method, path, bytes.NewReader(raw))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+token)
			out := httptest.NewRecorder()
			h.ServeHTTP(out, r)
			if out.Code != http.StatusForbidden {
				t.Fatalf("status=%d %s", out.Code, out.Body)
			}
		})
	}
}
