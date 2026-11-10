package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/providers"
)

func TestTerminalReportReplayResolvesCurrentCredentialsAndRetainedAttribution(t *testing.T) {
	l := layoutFor(initDemo(t))
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos = circuitBreakerTestConfig().Repos
	if err := instance.WriteConfig(l.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BLOCKED_TOK", "rotated-token")
	fake := &attributionRecordingCommenter{}
	old := newEscalationPoster
	newEscalationPoster = func(token string) gate.Commenter {
		if token != "rotated-token" {
			t.Errorf("replay did not resolve fresh credentials")
		}
		return fake
	}
	t.Cleanup(func() { newEscalationPoster = old })
	e := terminalreport.Effect{Kind: terminalreport.WorkFailure, Repository: circuitBreakerTestRepo(), ItemID: "42", Count: 1, RunID: "retained-run", Attribution: providers.Attribution{Schema: 1, Goobers: true, Gaggle: "example", Goober: "runner", Run: "retained-run", Workflow: "implementation", Task: "ci-poll"}}
	// No run journal or claims remain; captured attribution still permits replay.
	if err := replayTerminalReport(context.Background(), l, journal.NewRegistryScrubber(), nil, e); err != nil {
		t.Fatal(err)
	}
	if len(fake.attributions) == 0 || fake.attributions[0].Run != "retained-run" {
		t.Fatalf("missing attribution: %+v", fake.attributions)
	}
}

func TestTerminalReportReplayADOCommentAfterQuota(t *testing.T) {
	root, repo := adoConfiguredPATFixture(t)
	l := layoutFor(root)
	var mu sync.Mutex
	limited := true
	var posted []string
	const read = `{"id":42,"rev":1,"fields":{"System.WorkItemType":"Issue","System.State":"To Do"},"value":[{"name":"To Do","category":"Proposed"}],"comments":[]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if limited {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(429)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			posted = append(posted, payload.Text)
		}
		_, _ = w.Write([]byte(read))
	}))
	defer server.Close()
	old := newConfiguredADOProvider
	newConfiguredADOProvider = func(_ string, r providers.RepositoryRef) (*providers.ADOProvider, error) {
		if r != repo {
			t.Errorf("wrong repository: %+v", r)
		}
		return providers.NewADOProvider(repo.Owner, repo.Project, "test-token", providers.WithADOMaxRateLimitRetries(0), func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newConfiguredADOProvider = old })
	now := time.Now()
	q := newTerminalReportQueue(l)
	q.Now = func() time.Time { return now }
	e := terminalreport.Effect{Kind: terminalreport.WorkFailure, Repository: repo, ItemID: "42", Count: 2, RunID: "run-ado", Attribution: providers.Attribution{Schema: 1, Goobers: true, Gaggle: "example", Goober: "runner", Run: "run-ado", Workflow: "implementation", Task: "ci-poll"}}
	apply := func(ctx context.Context, e terminalreport.Effect) error {
		return replayTerminalReport(ctx, l, journal.NewRegistryScrubber(), nil, e)
	}
	if err := q.Publish(context.Background(), e, apply); err == nil {
		t.Fatal("expected deferred ADO report")
	}
	mu.Lock()
	limited = false
	mu.Unlock()
	now = now.Add(2 * time.Minute)
	if err := q.Reconcile(context.Background(), 8, apply); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 || !strings.Contains(posted[0], `data-count="2"`) {
		t.Fatalf("posted=%v", posted)
	}
	if a, ok, err := providers.ParseAttribution(posted[0]); err != nil || !ok || a.Run != "run-ado" {
		t.Fatalf("attribution=%+v ok=%v err=%v", a, ok, err)
	}
}

func TestTerminalReportWorkerStopsAndHonorsReadiness(t *testing.T) {
	l := layoutFor(initDemo(t))
	q := newTerminalReportQueue(l)
	// A pending report remains durable when the daemon stops.
	e := terminalreport.Effect{Kind: terminalreport.WorkFailure, ItemID: "42", RunID: "missing-run"}
	if err := q.Publish(context.Background(), e, func(context.Context, terminalreport.Effect) error { return errors.New("offline") }); err == nil {
		t.Fatal("expected pending")
	}
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := startTerminalReportRetry(ctx, l, journal.NewRegistryScrubber(), nil, log, true)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	disabled := startTerminalReportRetry(context.Background(), l, nil, nil, nil, false)
	select {
	case <-disabled:
	default:
		t.Fatal("unready worker started")
	}
}

func TestTerminalReportAbortLabelWaitsForFinalization(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: "run-finalizing", Workflow: "implementation"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jr.Close() }()
	e := terminalreport.Effect{Kind: terminalreport.AbortLabel, RunID: "run-finalizing", ItemID: "42"}
	err = replayTerminalAbortLabel(context.Background(), l, nil, nil, nil, apiv1.RepoRef{}, e)
	if err == nil || errors.Is(err, terminalreport.ErrSuperseded) {
		t.Fatalf("pending finalization dropped report: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := replayTerminalAbortLabel(context.Background(), l, nil, nil, nil, apiv1.RepoRef{}, e); !errors.Is(err, terminalreport.ErrSuperseded) {
		t.Fatalf("completed run report retained: %v", err)
	}
}
