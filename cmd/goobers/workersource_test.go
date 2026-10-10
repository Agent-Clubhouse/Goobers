package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
	"sigs.k8s.io/yaml"
)

func sourceBacklogFixture(t *testing.T, baseURL string) (instance.Layout, string, string, string) {
	t.Helper()
	root, marker := initRunDurationDemo(t)
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos = []instance.RepoRef{{Provider: "gitea", Owner: "acme", Name: "widgets", BaseURL: baseURL, Token: instance.TokenRef{Env: "SOURCE_BACKLOG_TOKEN"}}}
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	gagglePath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "gaggle.yaml")
	raw, err := os.ReadFile(gagglePath)
	if err != nil {
		t.Fatal(err)
	}
	var gaggle apiv1.Gaggle
	if err := yaml.Unmarshal(raw, &gaggle); err != nil {
		t.Fatal(err)
	}
	gaggle.Spec.Project = apiv1.RepoRef{Provider: apiv1.ProviderGitea, BaseURL: baseURL, Owner: "acme", Name: "widgets", Branch: "main"}
	gaggle.Spec.Backlog = apiv1.BacklogRef{Provider: apiv1.ProviderGitea, BaseURL: baseURL, Project: "acme/widgets"}
	raw, err = yaml.Marshal(map[string]any{"apiVersion": gaggle.APIVersion, "kind": gaggle.Kind, "metadata": map[string]string{"name": gaggle.Name}, "spec": gaggle.Spec})
	if err != nil {
		t.Fatal(err)
	}
	writeFileContent(t, gagglePath, string(raw))
	workflowPath := filepath.Join(filepath.Dir(gagglePath), "workflows", "default-implement.yaml")
	raw, err = os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	workflow := strings.Replace(string(raw), "    - type: manual", "    - type: backlog-item", 1)
	workflow = strings.Replace(workflow, "  start: probe", "  readiness:\n    maxConcurrentRuns: 2\n  start: probe", 1)
	writeFileContent(t, workflowPath, workflow)
	if code, out, stderr := runArgs(t, "validate", root); code != 0 {
		t.Fatalf("fixture validation: %s %s", out, stderr)
	}
	return layout, marker, workflowPath, workflow
}

func TestSourceBacklogProviderPollReopenExecutesCapturedWorkers(t *testing.T) {
	t.Setenv("SOURCE_BACKLOG_TOKEN", "queue-provider-fixture")
	var polls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/acme/widgets/issues" || r.Header.Get("Authorization") != "token queue-provider-fixture" {
			t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		polls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"number":11,"title":"one","state":"open"},{"number":12,"title":"two","state":"open"},{"number":13,"title":"three","state":"open"}]`))
	}))
	t.Cleanup(provider.Close)
	layout, marker, path, workflow := sourceBacklogFixture(t, provider.URL)
	first := openSourceDaemonFixture(t, layout)
	now := time.Now().UTC()
	first.scheduler.Tick(t.Context(), now)
	pending, err := first.service.queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 2 || polls.Load() == 0 {
		t.Fatalf("pending=%v polls=%d err=%v", pending, polls.Load(), err)
	}
	for _, record := range pending {
		envelope, err := startintent.Parse(record.Payload)
		if err != nil || envelope.Source == nil || envelope.Source.WorkerKind != "backlog" || envelope.Source.ObservedCount != 3 {
			t.Fatal(envelope, err)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker executed before dispatch: %v", err)
	}
	first.close()
	replacement := strings.Replace(workflow, strconv.Quote(marker), strconv.Quote(marker+".replacement"), 1)
	if replacement == workflow {
		t.Fatal("replacement unchanged")
	}
	writeFileContent(t, path, replacement)
	restarted := openSourceDaemonFixture(t, layout)
	restarted.scheduler.Tick(t.Context(), now.Add(time.Minute))
	repeated, err := restarted.service.queue.Pending(t.Context(), 10)
	if err != nil || len(repeated) != 2 {
		t.Fatal(repeated, err)
	}
	for range 3 {
		if err := restarted.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
		restarted.scheduler.Wait()
		restarted.workers.Wait()
	}
	for _, accepted := range pending {
		record, err := restarted.service.queue.Get(t.Context(), accepted.ID, "scheduler")
		if err != nil || record.State != triggerqueue.Dispatched {
			t.Fatal(record, err)
		}
		dir, err := layout.FindRunDir(record.RunID)
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
		if err := startintent.VerifyIdentity(id, record); err != nil {
			t.Fatal(err)
		}
		phase, err := reader.Phase()
		if err != nil || phase != journal.PhaseCompleted {
			t.Fatal(phase, err)
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker + ".replacement"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("executed replacement: %v", err)
	}
}
