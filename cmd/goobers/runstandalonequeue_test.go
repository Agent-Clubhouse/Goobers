package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

func standaloneQueue(t *testing.T, root string) *triggerqueue.Store {
	t.Helper()
	queue, err := triggerqueue.Open(filepath.Join(instance.NewLayout(root).SchedulerDir(), "accepted-triggers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	return queue
}
func TestStandaloneQueueCLIReplaysReceiptWithoutExecutingUnrelatedStarts(t *testing.T) {
	root, _ := initRunDurationDemo(t)
	code, stdout, stderr := runArgs(t, "run", "--request-id", "first", "default-implement", root)
	if code != 0 {
		t.Fatal(code, stdout, stderr)
	}
	queue := standaloneQueue(t, root)
	first, err := queue.ByKey(t.Context(), "first")
	if err != nil || first.RunID == "" || !strings.Contains(stdout, "created run "+first.RunID) {
		t.Fatal(first, stdout, err)
	}
	envelope, err := startintent.Parse(first.Payload)
	if err != nil {
		t.Fatal(err)
	}
	unrelated, _, err := queue.Accept(t.Context(), "unrelated", "standalone-cli", first.Payload, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// An accepted run remains pinned when the authored tree has another generation.
	source := filepath.Join(root, "config/gaggles/example/workflows/default-implement.yaml")
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	writeFileContent(t, source, string(body)+"\n# another authored generation\n")
	code, stdout, stderr = runArgs(t, "run", "--request-id", "first", "default-implement", root)
	if code != 0 || !strings.Contains(stdout, "created run "+first.RunID) {
		t.Fatal(code, stdout, stderr)
	}
	replay, err := queue.ByKey(t.Context(), "first")
	if err != nil || string(replay.Payload) != string(first.Payload) || replay.RunID != first.RunID || replay.State != triggerqueue.Dispatched {
		t.Fatal(replay, err)
	}
	code, stdout, stderr = runArgs(t, "run", "--request-id", "second", "default-implement", root)
	if code != 0 {
		t.Fatal(code, stdout, stderr)
	}
	untouched, err := queue.Get(t.Context(), unrelated.ID, "standalone-cli")
	if err != nil || untouched.State != triggerqueue.Accepted {
		t.Fatal(untouched, err)
	}
	runs, err := os.ReadDir(instance.NewLayout(root).ForGaggle(envelope.Target.Gaggle).RunsDir())
	if err != nil || len(runs) != 2 {
		t.Fatal("unexpected workflow execution", len(runs), err)
	}
	code, _, stderr = runArgs(t, "run", "--request-id", "first", "--force", "default-implement", root)
	if code != 1 || !strings.Contains(stderr, "another request") {
		t.Fatal(code, stderr)
	}
}

func TestStandaloneQueueCapacityReceiptSurvivesAndUsesActualPinnedRunner(t *testing.T) {
	f := sourceHost(t, "    - type: manual")
	release, ok, reason := f.sched.ReserveContinuation("held-run", f.entry.Gaggle, f.entry.Workflow)
	if !ok {
		t.Fatal(reason)
	}
	defer release()
	target := runTarget{Gaggle: f.entry.Gaggle, Workflow: f.entry.Workflow, RequestID: "held"}
	var output bytes.Buffer
	runID, err := dispatchStandaloneStart(t.Context(), f.layout, f.setup, f.sched, target, &output)
	if err == nil || runID != "" || !strings.Contains(output.String(), "accepted trigger") {
		t.Fatal(runID, output.String(), err)
	}
	record, err := f.service.queue.ByKey(t.Context(), target.RequestID)
	if err != nil || record.State != triggerqueue.Accepted {
		t.Fatal(record, err)
	}
	release()
	writeFileContent(t, f.source, "invalid unsaved source: [")
	runID, err = dispatchStandaloneStart(t.Context(), f.layout, f.setup, f.sched, target, &output)
	if err != nil || runID != strings.TrimPrefix(record.ID, "trigger-") {
		t.Fatal(runID, err)
	}
	f.sched.Wait()
	f.wg.Wait()
	dir, err := f.layout.FindRunDir(runID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err = startintent.VerifyIdentity(id, record); err != nil {
		t.Fatal(err)
	}
	phase, err := reader.Phase()
	if err != nil || phase != journal.PhaseCompleted {
		t.Fatal(phase, err)
	}
}

func TestStandaloneQueueFailureNeverLaunches(t *testing.T) {
	root, marker := initRunDurationDemo(t)
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "run", "default-implement", root)
	if code == 0 || strings.Contains(stdout, "created run ") {
		t.Fatal(code, stdout, stderr)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("workflow launched without durable queue", err)
	}
}

func TestDetachedWorkerCarriesRequestIdentityAndReplaysActualRun(t *testing.T) {
	root, _ := initRunDurationDemo(t)
	target := runTarget{Workflow: "default-implement", RequestID: "caller/key with spaces#and-fragments"}
	selector := detachedRunSelector(target)
	name, key, err := splitDetachedRequest(selector)
	if err != nil || name != target.Workflow || key != target.RequestID {
		t.Fatal(name, key, err)
	}
	for range 2 {
		var stdout, stderr bytes.Buffer
		if code := runDetachedWorkerContext(context.Background(), []string{selector, root}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "created run ") {
			t.Fatal(code, stdout.String(), stderr.String())
		}
	}
	queue := standaloneQueue(t, root)
	record, err := queue.ByKey(t.Context(), target.RequestID)
	if err != nil || record.State != triggerqueue.Dispatched {
		t.Fatal(record, err)
	}
	runs, err := os.ReadDir(instance.NewLayout(root).ForGaggle("example").RunsDir())
	if err != nil || len(runs) != 1 {
		t.Fatal(len(runs), err)
	}
}

func TestStandaloneQueueTargetedPRUsesCurrentConfiguredCredential(t *testing.T) {
	const token = "standalone-configured-pr-token"
	root := initTargetedMergeReviewDemo(t, token)
	var closed atomic.Bool
	var requests atomic.Int32
	previous := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(provider *providers.GitHubProvider) {
			provider.Client = scopedStageReadTransport(func(req *http.Request) (*http.Response, error) {
				requests.Add(1)
				if req.Method != http.MethodGet || req.URL.Path != "/repos/your-org/your-repo/pulls/3261" || req.Header.Get("Authorization") != "Bearer standalone-configured-pr-token" {
					t.Errorf("unexpected provider request: %s %s (configured credential=%t)", req.Method, req.URL.Path, req.Header.Get("Authorization") == "Bearer standalone-configured-pr-token")
					return nil, errors.New("unexpected provider request")
				}
				state := "open"
				if closed.Load() {
					state = "closed"
				}
				body := `{"number":3261,"state":"` + state + `","html_url":"https://github.com/your-org/your-repo/pull/3261"}`
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
		})...)
	}
	t.Cleanup(func() { newGitHubProvider = previous })
	code, stdout, stderr := runArgs(t, "run", "merge-review", "--pr", "3261", "--no-wait", "--request-id", "pr-open", root)
	if code != 0 {
		t.Fatal(code, stdout, stderr)
	}
	queue := standaloneQueue(t, root)
	record, err := queue.ByKey(t.Context(), "pr-open")
	if err != nil || record.RunID == "" || !strings.Contains(stdout, "created run "+record.RunID) {
		t.Fatal(record, stdout, err)
	}
	waitForTargetedRunCleanup(t, root, record.RunID)
	reader, err := journal.OpenReadOnly(filepath.Join(instance.NewLayout(root).ForGaggle("example").RunsDir(), record.RunID))
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil || id.Trigger.Kind != journal.TriggerSignal || id.Trigger.Ref != "github-webhook:pull_request#3261" {
		t.Fatal(id, err)
	}
	closed.Store(true)
	code, _, stderr = runArgs(t, "run", "merge-review", "--pr", "3261", "--no-wait", "--request-id", "pr-closed", root)
	if code != 1 || !strings.Contains(stderr, "closed") || requests.Load() != 2 || targetedRunCount(t, root) != 1 {
		t.Fatal(code, stderr, requests.Load(), targetedRunCount(t, root))
	}
	rejected, err := queue.ByKey(t.Context(), "pr-closed")
	if err != nil || rejected.State != triggerqueue.Rejected {
		t.Fatal(rejected, err)
	}
}
