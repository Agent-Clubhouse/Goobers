package hostedprogress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestEnvironmentRequiresActionsContract(t *testing.T) {
	for _, name := range []string{"GITHUB_REPOSITORY", "GITHUB_SHA", "GITHUB_RUN_ID", "GITHUB_TOKEN", "GH_TOKEN"} {
		t.Setenv(name, "")
	}
	_, err := Environment()
	if err == nil || !strings.Contains(err.Error(), "GITHUB_REPOSITORY") ||
		!strings.Contains(err.Error(), "checks: write") {
		t.Fatalf("Environment() error = %v", err)
	}
}

func TestEnvironmentUsesGitHubDefaults(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "owner/repo")
	t.Setenv("GITHUB_SHA", "deadbeef")
	t.Setenv("GITHUB_RUN_ID", "123")
	t.Setenv("GITHUB_TOKEN", "token")
	t.Setenv("GITHUB_API_URL", "")
	t.Setenv("GITHUB_SERVER_URL", "")
	env, err := Environment()
	if err != nil {
		t.Fatal(err)
	}
	if env.APIURL != "https://api.github.com" || env.ServerURL != "https://github.com" {
		t.Fatalf("Environment() URLs = %q, %q", env.APIURL, env.ServerURL)
	}
}

func TestPublisherCreatesUpdatesAndDeduplicatesCheckRun(t *testing.T) {
	var mu sync.Mutex
	var writes []map[string]any
	var lookups int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			mu.Lock()
			lookups++
			mu.Unlock()
			// No pre-existing Check Run for this SHA/name/external_id, so
			// the publisher must fall through to POST create.
			_, _ = io.WriteString(w, `{"check_runs":[]}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode %s request: %v", r.Method, err)
		}
		mu.Lock()
		writes = append(writes, body)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	defer server.Close()

	runDir, events := testJournal(t)
	publisher := New(GitHubEnvironment{
		Repository:   "owner/repo",
		SHA:          "deadbeef",
		ActionsRunID: "123",
		Token:        "token",
		APIURL:       server.URL,
		ServerURL:    "https://github.example",
	}, runDir)

	if err := publisher.Publish(context.Background(), events[:1]); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), events[:1]); err != nil {
		t.Fatal(err)
	}
	heartbeatEvents := append([]journal.Event(nil), events[:1]...)
	heartbeatEvents = append(heartbeatEvents, journal.Event{
		Seq:  events[0].Seq + 1,
		Type: journal.EventStageHeartbeat,
	})
	if err := publisher.Publish(context.Background(), heartbeatEvents); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), events); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if lookups != 1 {
		t.Fatalf("Check Run lookups = %d, want 1 (only on the first Publish)", lookups)
	}
	if len(writes) != 2 {
		t.Fatalf("writes = %d, want 2", len(writes))
	}
	if writes[0]["external_id"] != Schema+":0123456789abcdef0123456789abcdef:123" {
		t.Fatalf("external_id = %v", writes[0]["external_id"])
	}
	output := writes[0]["output"].(map[string]any)
	text := output["text"].(string)
	if !strings.Contains(text, startMarker) || !strings.Contains(text, `"schema":"`+Schema+`"`) {
		t.Fatalf("check output does not contain hosted progress contract: %s", text)
	}
	if writes[1]["status"] != "completed" || writes[1]["conclusion"] != "success" {
		t.Fatalf("terminal update = %#v", writes[1])
	}
}

// TestPublisherReusesExistingCheckRunAcrossPublishers pins the recovery
// contract: a Publisher spun up after a process restart, retry, or replay
// (checkID = 0, but a Check Run for our stable external_id already exists on
// this SHA) MUST look up that Check Run and PATCH it, not POST a duplicate.
// See internal/hostedprogress/progress.go findExisting and the Publish
// checkID == 0 branch.
func TestPublisherReusesExistingCheckRunAcrossPublishers(t *testing.T) {
	const existingCheckID int64 = 42
	var (
		mu           sync.Mutex
		methods      []string
		createExtID  string
		patchedIDs   []int64
		listQueries  []string
		listResponse atomic.Value // string
	)
	listResponse.Store(`{"check_runs":[]}`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			mu.Lock()
			listQueries = append(listQueries, r.URL.RawQuery)
			mu.Unlock()
			_, _ = io.WriteString(w, listResponse.Load().(string))
		case http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode POST: %v", err)
			}
			mu.Lock()
			createExtID, _ = body["external_id"].(string)
			mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"id":%d}`, existingCheckID)
			// Publisher A's create means a future GET must return the
			// existing Check Run so Publisher B can find it.
			listResponse.Store(fmt.Sprintf(
				`{"check_runs":[{"id":%d,"external_id":%q}]}`,
				existingCheckID, createExtID,
			))
		case http.MethodPatch:
			parts := strings.Split(r.URL.Path, "/")
			var id int64
			_, _ = fmt.Sscanf(parts[len(parts)-1], "%d", &id)
			mu.Lock()
			patchedIDs = append(patchedIDs, id)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":42}`)
		default:
			t.Errorf("unexpected method %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	env := GitHubEnvironment{
		Repository:   "owner/repo",
		SHA:          "deadbeef",
		ActionsRunID: "123",
		Token:        "token",
		APIURL:       server.URL,
		ServerURL:    "https://github.example",
	}
	runDir, events := testJournal(t)

	first := New(env, runDir)
	if err := first.Publish(context.Background(), events[:1]); err != nil {
		t.Fatalf("first publisher Publish: %v", err)
	}

	// Fresh Publisher instance — simulates a process restart. checkID is 0
	// in memory but a Check Run with the stable external_id exists on the
	// SHA, so this Publish MUST reuse it via PATCH, not create a duplicate.
	second := New(env, runDir)
	if err := second.Publish(context.Background(), events); err != nil {
		t.Fatalf("second publisher Publish: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	posts := 0
	patches := 0
	gets := 0
	for _, m := range methods {
		switch {
		case strings.HasPrefix(m, "POST "):
			posts++
		case strings.HasPrefix(m, "PATCH "):
			patches++
		case strings.HasPrefix(m, "GET "):
			gets++
		}
	}
	if posts != 1 {
		t.Fatalf("POST count = %d, want exactly 1 (second Publisher must not create a duplicate); methods=%v", posts, methods)
	}
	if patches != 1 {
		t.Fatalf("PATCH count = %d, want 1 (second Publisher must update the existing Check Run); methods=%v", patches, methods)
	}
	if gets != 2 {
		t.Fatalf("GET count = %d, want 2 (one per fresh Publisher); methods=%v", gets, methods)
	}
	for _, q := range listQueries {
		want := "check_name=" + url.QueryEscape(CheckPrefix+"implement-locally")
		if !strings.Contains(q, want) {
			t.Fatalf("list query %q missing %q — findExisting must scope by check_name to keep the client-side scan bounded", q, want)
		}
		if !strings.Contains(q, "per_page=100") {
			t.Fatalf("list query %q missing per_page=100 — findExisting must request the API cap so it never silently misses a match", q)
		}
	}
	if len(patchedIDs) != 1 || patchedIDs[0] != existingCheckID {
		t.Fatalf("PATCHed IDs = %v, want [%d] — second Publisher must reuse the existing Check Run", patchedIDs, existingCheckID)
	}
	wantExtID := Schema + ":0123456789abcdef0123456789abcdef:123"
	if createExtID != wantExtID {
		t.Fatalf("create external_id = %q, want %q", createExtID, wantExtID)
	}
}

// TestPublisherFindExistingErrorDisablesPublisher pins the recovery-path
// error handling: a failing lookup must NOT be swallowed — it disables the
// Publisher exactly like a failing create/update, so callers observe the
// GitHub API failure and stop hammering it.
func TestPublisherFindExistingErrorDisablesPublisher(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s after failing lookup", r.Method)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	}))
	defer server.Close()

	runDir, events := testJournal(t)
	publisher := New(GitHubEnvironment{
		Repository:   "owner/repo",
		SHA:          "deadbeef",
		ActionsRunID: "123",
		Token:        "token",
		APIURL:       server.URL,
		ServerURL:    "https://github.example",
	}, runDir)

	err := publisher.Publish(context.Background(), events)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("Publish err = %v, want lookup-failure surfaced", err)
	}

	// A subsequent Publish must return the same disabled error without
	// contacting GitHub again — no fallback to create, no retry loop.
	before := atomic.LoadInt32(&calls)
	err2 := publisher.Publish(context.Background(), events)
	if err2 == nil || !errors.Is(err2, err) && err2.Error() != err.Error() {
		t.Fatalf("second Publish err = %v, want same disabled error", err2)
	}
	if atomic.LoadInt32(&calls) != before {
		t.Fatalf("second Publish contacted GitHub again after lookup failure: calls before=%d after=%d", before, atomic.LoadInt32(&calls))
	}
}

func TestBoundContractPreservesLatestEvents(t *testing.T) {
	events := []journal.Event{{Seq: 1, Type: journal.EventRunStarted}}
	for i := 2; i < 1000; i++ {
		events = append(events, journal.Event{
			Seq:  uint64(i),
			Type: journal.EventError,
			Error: &journal.ErrorDetail{
				Code:    "large",
				Message: strings.Repeat("x", 200),
			},
		})
	}

	contract := Contract{Schema: Schema, Events: events}
	boundContract(&contract)
	raw, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxPayloadBytes {
		t.Fatalf("bounded payload = %d bytes", len(raw))
	}
	if contract.Events[0].Type != journal.EventRunStarted ||
		contract.Events[len(contract.Events)-1].Seq != 999 ||
		contract.TruncatedBefore == 0 {
		t.Fatalf("bounded events did not preserve start/latest: %#v", contract)
	}
}

func TestBoundContractMarksTruncationWhenTwoEventsBecomeOne(t *testing.T) {
	events := []journal.Event{
		{
			Seq:   1,
			Type:  journal.EventError,
			Error: &journal.ErrorDetail{Code: "large", Message: strings.Repeat("x", 30_000)},
		},
		{
			Seq:   2,
			Type:  journal.EventError,
			Error: &journal.ErrorDetail{Code: "large", Message: strings.Repeat("y", 30_000)},
		},
	}
	contract := Contract{Schema: Schema, Revision: 2, Events: events}

	boundContract(&contract)

	if len(contract.Events) != 1 || contract.Events[0].Seq != 1 || contract.TruncatedBefore != 2 {
		t.Fatalf("two-event truncation = %#v", contract)
	}
}

func TestConclusionMapping(t *testing.T) {
	tests := map[journal.RunPhase]string{
		journal.PhaseCompleted: "success",
		journal.PhaseFailed:    "failure",
		journal.PhaseAborted:   "cancelled",
		journal.PhaseEscalated: "action_required",
	}
	for phase, want := range tests {
		if got := conclusion(phase); got != want {
			t.Errorf("conclusion(%q) = %q, want %q", phase, got, want)
		}
	}
}

// TestFinalizeConclusionMaps pins the wait-error-to-Check-Run-conclusion
// contract Finalize uses on abnormal exit. Cancelled or deadline-exceeded
// waits map to "cancelled" (a signal-driven or job-timeout shutdown is the
// Actions user's decision, not a run failure) while every other wait error
// maps to "failure" (a wedged watcher or hosted-progress publisher error is
// operator-visible).
func TestFinalizeConclusionMaps(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"nil":                       {nil, "cancelled"},
		"context cancelled":         {context.Canceled, "cancelled"},
		"context deadline exceeded": {context.DeadlineExceeded, "cancelled"},
		"other error":               {errors.New("wait wedged"), "failure"},
		"wrapped cancelled":         {fmt.Errorf("outer: %w", context.Canceled), "cancelled"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := finalizeConclusion(tc.err); got != tc.want {
				t.Fatalf("finalizeConclusion(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestPublisherFinalizeIsNoOpBeforeCreate pins the best-effort promise: if
// Publish never got as far as creating a Check Run (because the run exited
// abnormally before the first journal transition, or because Publish
// latched an error on the create attempt itself) Finalize must not issue
// a stray PATCH — there is nothing to complete.
func TestPublisherFinalizeIsNoOpBeforeCreate(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	defer server.Close()

	publisher := New(GitHubEnvironment{
		Repository:   "owner/repo",
		SHA:          "deadbeef",
		ActionsRunID: "123",
		Token:        "token",
		APIURL:       server.URL,
		ServerURL:    "https://github.example",
	}, t.TempDir())

	if err := publisher.Finalize(context.Background(), errors.New("wait wedged")); err != nil {
		t.Fatalf("Finalize before create: %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("Finalize before create issued %d requests, want 0", got)
	}
}

// TestPublisherFinalizeClosesCheckRunOnAbnormalExit pins the review-flagged
// lifecycle guarantee: if Publish created a Check Run but the caller exited
// before any terminal phase was published, Finalize must complete the
// Check Run so it does not linger at "in progress". Signal-driven cancel
// maps to "cancelled"; a wait error maps to "failure"; a second call after
// finalize is a no-op.
func TestPublisherFinalizeClosesCheckRunOnAbnormalExit(t *testing.T) {
	var mu sync.Mutex
	var writes []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			// No pre-existing Check Run for this SHA — force create path.
			_, _ = io.WriteString(w, `{"check_runs":[]}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode %s request: %v", r.Method, err)
		}
		mu.Lock()
		writes = append(writes, body)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	defer server.Close()

	runDir, events := testJournal(t)
	// Trim the terminal event so Publish creates a Check Run in the
	// "in_progress" state (matching a live run that hasn't finished yet).
	inFlight := events[:len(events)-1]
	if len(inFlight) == 0 {
		inFlight = events[:1]
	}
	publisher := New(GitHubEnvironment{
		Repository:   "owner/repo",
		SHA:          "deadbeef",
		ActionsRunID: "123",
		Token:        "token",
		APIURL:       server.URL,
		ServerURL:    "https://github.example",
	}, runDir)

	if err := publisher.Publish(context.Background(), inFlight); err != nil {
		t.Fatalf("initial publish: %v", err)
	}

	if err := publisher.Finalize(context.Background(), context.Canceled); err != nil {
		t.Fatalf("Finalize on cancel: %v", err)
	}
	if err := publisher.Finalize(context.Background(), errors.New("second call")); err != nil {
		t.Fatalf("Finalize idempotent call: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(writes) != 2 {
		t.Fatalf("writes = %d, want 2 (one create, one finalize PATCH)", len(writes))
	}
	terminal := writes[1]
	if terminal["status"] != "completed" || terminal["conclusion"] != "cancelled" {
		t.Fatalf("finalize PATCH = %#v, want completed/cancelled", terminal)
	}
}

func TestBoundContractCompactsSingleOversizedEvent(t *testing.T) {
	contract := Contract{
		Schema:   Schema,
		Revision: 1,
		Events: []journal.Event{{
			Seq:  1,
			Type: journal.EventError,
			Error: &journal.ErrorDetail{
				Code:    "large",
				Message: strings.Repeat("x", maxPayloadBytes*2),
			},
		}},
	}
	boundContract(&contract)
	raw, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxPayloadBytes || len(contract.Events) != 1 ||
		len(contract.Events[0].Error.Message) > 1027 {
		t.Fatalf("single event was not compacted: bytes=%d event=%#v", len(raw), contract.Events)
	}
}

// TestBoundContractMarksAllEventsDropped pins the all-events-dropped bounding
// state: when even a compacted single event cannot fit alongside the
// non-event portion of the contract (notably an unbounded Identity.Inputs),
// boundContract drops the last event, records TruncatedBefore = Revision, and
// leaves Events as an empty non-nil slice so the payload still marshals to
// `"events": []` — the shape the published JSON schema declares as required
// (see api/validate/hosted_progress_schema_test.go
// TestHostedProgressAllEventsDroppedContractValidates for the schema-side
// coverage).
func TestBoundContractMarksAllEventsDropped(t *testing.T) {
	// One InputRef.Name blob strictly larger than maxPayloadBytes forces the
	// non-event portion to exceed the budget on its own, so no single event
	// (even after compactEvent) can fit alongside it — driving boundContract
	// through the compaction step into the total-drop branch.
	contract := Contract{
		Schema:   Schema,
		Revision: 7,
		Identity: journal.RunIdentity{
			Inputs: []journal.InputRef{{Name: strings.Repeat("x", maxPayloadBytes*2)}},
		},
		Events: []journal.Event{{Seq: 7, Type: journal.EventRunFinished}},
	}

	boundContract(&contract)

	if contract.Events == nil {
		t.Fatal("Events must be an empty non-nil slice after total drop; nil marshals to \"events\": null and violates the schema")
	}
	if len(contract.Events) != 0 {
		t.Fatalf("total-drop branch retained events: %#v", contract.Events)
	}
	if contract.TruncatedBefore != contract.Revision {
		t.Fatalf("TruncatedBefore = %d, want %d (= Revision)", contract.TruncatedBefore, contract.Revision)
	}

	raw, err := json.Marshal(contract)
	if err != nil {
		t.Fatalf("marshal all-dropped contract: %v", err)
	}
	if !strings.Contains(string(raw), `"events":[]`) {
		t.Fatalf("marshaled payload must contain empty events array, got: %s", raw)
	}
	if strings.Contains(string(raw), `"events":null`) {
		t.Fatalf("marshaled payload must not contain \"events\":null, got: %s", raw)
	}
}

func testJournal(t *testing.T) (string, []journal.Event) {
	t.Helper()
	runsDir := t.TempDir()
	runID := "0123456789abcdef0123456789abcdef"
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implement-locally",
		WorkflowVersion: 1,
		Gaggle:          "crawler",
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
		StartedAt:       time.Now().UTC(),
	}, map[string][]byte{
		journal.PinnedWorkflowGraphInputName: []byte(`{"nodes":[{"id":"plan"}],"edges":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type:   journal.EventRunFinished,
		Status: string(journal.PhaseCompleted),
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(runsDir, runID), events
}
