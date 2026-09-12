package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
)

type recordingDivergenceAppender struct {
	events chan journal.Event
}

func (r *recordingDivergenceAppender) Append(event journal.Event) error {
	if r.events != nil {
		r.events <- event
	}
	return nil
}

type failFirstDivergenceAppender struct {
	calls    atomic.Int32
	attempts chan string
}

func (a *failFirstDivergenceAppender) Append(event journal.Event) error {
	a.attempts <- divergenceRunnerString(event, "state")
	if a.calls.Add(1) == 1 {
		return errors.New("response lost")
	}
	return nil
}

func TestWorkerDivergenceRetriesFailedTransitionBeforeNewerState(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		digest := "sha256:worker"
		if reads.Add(1) > 1 {
			digest = "sha256:new"
		}
		_, _ = fmt.Fprintf(w, `{"digest":%q}`, digest)
	}))
	t.Cleanup(server.Close)
	seams := &workerSeams{logf: func(string, ...any) {}}
	seams.snapshot.Store(&workerConfigSnapshot{digest: "sha256:worker"})
	appender := &failFirstDivergenceAppender{attempts: make(chan string, 8)}
	watcher := startWorkerDivergenceWatcher(context.Background(), seams, server.Client(), server.URL,
		func() (string, error) { return "token", nil }, 10*time.Millisecond, "worker:a", appender)
	t.Cleanup(watcher.Stop)
	want := []string{workerDivergenceInSync, workerDivergenceInSync, workerDivergenceDiverged}
	for i, expected := range want {
		select {
		case got := <-appender.attempts:
			if got != expected {
				t.Fatalf("attempt %d state = %q, want %q", i, got, expected)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("missing attempt %d", i)
		}
	}
}

func TestWorkerDigestMintFailureReportsUnavailableAndRecovers(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"digest":"sha256:original"}`)
	}))
	t.Cleanup(server.Close)
	var available atomic.Bool
	messages := make(chan string, 16)
	events := make(chan journal.Event, 16)
	seams := &workerSeams{logf: func(format string, args ...any) { messages <- fmt.Sprintf(format, args...) }}
	seams.snapshot.Store(&workerConfigSnapshot{digest: "sha256:original"})
	watcher := startWorkerDivergenceWatcher(context.Background(), seams, server.Client(), server.URL, func() (string, error) {
		if !available.Load() {
			return "", errors.New("credential unavailable")
		}
		return "restored", nil
	}, 10*time.Millisecond, "worker-a", &recordingDivergenceAppender{events: events})
	t.Cleanup(watcher.Stop)
	select {
	case message := <-messages:
		if !strings.Contains(message, "NOT CHECKED (credential unavailable)") || calls.Load() != 0 {
			t.Fatalf("credential failure issued a request or wrong report: %q, calls=%d", message, calls.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing credential failure report")
	}
	assertDivergenceEvent(t, events, "worker-a", workerDivergenceNotChecked)
	available.Store(true)
	select {
	case message := <-messages:
		if !strings.Contains(message, "divergence: none") {
			t.Fatalf("missing recovery: %q", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("credential recovery was not checked")
	}
	assertDivergenceEvent(t, events, "worker-a", workerDivergenceInSync)
	watcher.Stop()
	select {
	case <-watcher.done:
	default:
		t.Fatal("Stop returned before watcher finished")
	}
}

func assertDivergenceEvent(t *testing.T, events <-chan journal.Event, worker, state string) {
	t.Helper()
	select {
	case event := <-events:
		if event.Type != journal.EventWorkerConfigDivergence || event.Runner["worker"] != worker || event.Runner["state"] != state {
			t.Fatalf("divergence event = %+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("missing %s divergence event", state)
	}
}

func TestWorkerDigestKeyAuthenticatesPollingAndDetectsRecovery(t *testing.T) {
	root := initDemo(t)
	configureDispatchAuthority(t, root)
	source, err := workerDigestTokenSource(root, "")
	if err != nil || source == nil {
		t.Fatalf("shared-key watcher disabled: %v", err)
	}
	signer, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(signer, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	var requests, mints atomic.Int32
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(auth), httpapi.WithConfigDigest(func() string {
			if requests.Add(1) == 2 {
				return "sha256:changed"
			}
			return "sha256:original"
		}))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	messages := make(chan string, 16)
	events := make(chan journal.Event, 16)
	seams := &workerSeams{logf: func(format string, args ...any) { messages <- fmt.Sprintf(format, args...) }}
	seams.snapshot.Store(&workerConfigSnapshot{digest: "sha256:original"})
	watcher := startWorkerDivergenceWatcher(context.Background(), seams, server.Client(), server.URL, func() (string, error) { mints.Add(1); return source() }, 10*time.Millisecond, "worker-a", &recordingDivergenceAppender{events: events})
	t.Cleanup(watcher.Stop)
	for i, want := range []string{"divergence: none", "daemon has sha256:changed", "divergence: none"} {
		select {
		case message := <-messages:
			if !strings.Contains(message, want) {
				t.Fatalf("report %q does not contain %q", message, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("authenticated watcher did not report divergence and recovery")
		}
		assertDivergenceEvent(t, events, "worker-a", []string{workerDivergenceInSync, workerDivergenceDiverged, workerDivergenceInSync}[i])
	}
	watcher.Stop()
	// Stop may cancel the next poll after minting but before its HTTP request
	// reaches the server. Polls are serial, so at most one such mint can remain.
	if mints.Load() < 3 || mints.Load() < requests.Load() || mints.Load() > requests.Load()+1 {
		t.Fatalf("credentials not minted per poll: %d mints, %d reads", mints.Load(), requests.Load())
	}
	// The actual same credential is refused at a human run-list route.
	token, err := source()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, apicontract.RunsPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("worker run list = %d", response.Code)
	}
}

func TestWorkerDigestTokenSourceKeepsStaticAndUnconfiguredPostures(t *testing.T) {
	root := initDemo(t)
	source, err := workerDigestTokenSource(root, "")
	if err != nil || source != nil {
		t.Fatalf("unconfigured auth: source=%v, err=%v", source != nil, err)
	}
	source, err = workerDigestTokenSource("unreadable-instance", "explicit-static-token")
	if err != nil {
		t.Fatal(err)
	}
	token, err := source()
	if err != nil || token != "explicit-static-token" {
		t.Fatal("static token compatibility lost")
	}
	if _, err := workerDigestTokenSource("unreadable-instance", ""); err == nil {
		t.Fatal("unreadable configured instance silently disables authentication")
	}
}

func TestStaticPodTokenMayReadDigestButCannotForgeWorkerDivergence(t *testing.T) {
	registry := podauth.NewRegistry()
	token, err := registry.Mint("stage-run", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(auth),
		httpapi.WithConfigDigest(func() string { return "sha256:daemon" }),
		httpapi.WithWorkerConfigDivergence(func(journal.Event) error { t.Fatal("stage token appended worker state"); return nil }))
	if err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRequest(http.MethodGet, apicontract.ConfigDigestPath, nil)
	get.Header.Set("Authorization", "Bearer "+token)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("config digest status = %d, body=%s", getResponse.Code, getResponse.Body.String())
	}
	post := httptest.NewRequest(http.MethodPost, apicontract.WorkerConfigDivergencePath,
		bytes.NewBufferString(`{"state":"not-checked","reason":"forged"}`))
	post.Header.Set("Authorization", "Bearer "+token)
	postResponse := httptest.NewRecorder()
	handler.ServeHTTP(postResponse, post)
	if postResponse.Code != http.StatusForbidden {
		t.Fatalf("worker divergence status = %d, body=%s", postResponse.Code, postResponse.Body.String())
	}
}

func TestWorkerDigestFetchRefusesRedirectAndOversizedPayload(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		_, _ = io.WriteString(w, `{"digest":"leaked"}`)
	}))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	if _, err := fetchDaemonConfigDigest(context.Background(), redirect.Client(), redirect.URL, "worker-secret"); err == nil || targetCalls.Load() != 0 {
		t.Fatal("worker credential followed a redirect")
	}
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"digest":"`+strings.Repeat("x", 4096)+`"}`)
	}))
	t.Cleanup(large.Close)
	if _, err := fetchDaemonConfigDigest(context.Background(), large.Client(), large.URL, "worker-secret"); err == nil {
		t.Fatal("unbounded response accepted")
	}
}

func TestRemoteWorkerDivergenceAppenderUsesAuthoritativeDaemonPlane(t *testing.T) {
	var got map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != apicontract.WorkerConfigDivergencePath || request.Header.Get("Authorization") != "Bearer worker-token" {
			t.Fatalf("request = %s %s auth=%q", request.Method, request.URL.Path, request.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	appender := &remoteWorkerDivergenceAppender{client: server.Client(), baseURL: server.URL, tokenSource: func() (string, error) { return "worker-token", nil }}
	if err := appender.Append(journal.Event{Type: journal.EventWorkerConfigDivergence, Runner: map[string]any{
		"worker": "worker-a", "state": "not-checked", "reason": "digest unavailable",
	}}); err != nil {
		t.Fatal(err)
	}
	if got["state"] != "not-checked" || got["reason"] != "digest unavailable" || got["worker"] != "" || got["message"] != "" {
		t.Fatalf("request = %+v", got)
	}
}

func TestInactiveWorkerDivergenceIsAFirstClassTransition(t *testing.T) {
	events := make(chan journal.Event, 1)
	message, err := recordInactiveWorkerDivergence(&recordingDivergenceAppender{events: events}, "worker-a", "sha256:worker")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "NOT ACTIVE") {
		t.Fatalf("message = %q", message)
	}
	assertDivergenceEvent(t, events, "worker-a", workerDivergenceNotActive)
}

func TestWorkerDivergenceJournalRecorderDeduplicatesAcrossRestart(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	recorder, err := newWorkerDivergenceJournalRecorder(log)
	if err != nil {
		t.Fatal(err)
	}
	event := journal.Event{Type: journal.EventWorkerConfigDivergence, Runner: map[string]any{
		"worker": "worker:a", "state": workerDivergenceInSync,
		"workerDigest": "sha256:a", "daemonDigest": "sha256:a", "message": "in sync",
	}}
	if err := recorder.Append(event); err != nil {
		t.Fatal(err)
	}
	restarted, err := newWorkerDivergenceJournalRecorder(log)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Append(event); err != nil {
		t.Fatal(err)
	}
	event.Runner["state"] = workerDivergenceDiverged
	event.Runner["daemonDigest"] = "sha256:b"
	event.Runner["message"] = "diverged"
	if err := restarted.Append(event); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Runner["state"] != workerDivergenceInSync || events[1].Runner["state"] != workerDivergenceDiverged {
		t.Fatalf("events = %+v", events)
	}
}

func TestDaemonRecordsRemoteReportingNotActiveWithoutWorkerKey(t *testing.T) {
	events := make(chan journal.Event, 1)
	cfg := &instance.Config{Engine: &instance.EngineConfig{HostPort: "temporal:7233"}}
	if err := recordDaemonWorkerDivergenceAvailability(&recordingDivergenceAppender{events: events}, cfg); err != nil {
		t.Fatal(err)
	}
	assertDivergenceEvent(t, events, "worker:remote-reporting", workerDivergenceNotActive)
	cfg.API.PodTokenKeyFile = "/var/run/goobers/pod.key"
	if err := recordDaemonWorkerDivergenceAvailability(&recordingDivergenceAppender{events: events}, cfg); err != nil {
		t.Fatal(err)
	}
	assertDivergenceEvent(t, events, "worker:remote-reporting", workerDivergenceNotChecked)
}
