//go:build integration

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/test/testsupport/testdep"
	"sigs.k8s.io/yaml"
)

func TestIntegrationParallelParentsAuthorSeparateChildrenThroughRealWorkers(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	qualifyContainedParentJourney(t, "parallel")
}

func parallelQualificationDefinition(t *testing.T, source string) string {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(source), &doc); err != nil {
		t.Fatal(err)
	}
	spec := doc["spec"].(map[string]any)
	original, err := yaml.Marshal(spec["tasks"].([]any)[0])
	if err != nil {
		t.Fatal(err)
	}
	var tasks []any
	for _, name := range []string{"left", "right", "join"} {
		var task map[string]any
		if err := yaml.Unmarshal(original, &task); err != nil {
			t.Fatal(err)
		}
		task["name"], task["goal"], task["timeoutSeconds"] = name, "QUALIFICATION_BRANCH="+name, 180
		if name == "join" {
			task["repoFrom"] = []string{"plan", "right"}
		} else {
			task["next"] = "@join"
			if name == "left" {
				task["name"] = "plan" // Retain the shared compiler fixture's selected stage.
			}
		}
		tasks = append(tasks, task)
	}
	spec["tasks"], spec["start"] = tasks, "fan"
	// Both children charge their parent workflow budget. The overlap barrier
	// requires two slots rather than the default serial admission limit.
	spec["readiness"] = map[string]any{"maxConcurrentRuns": 2}
	spec["parallels"] = []any{map[string]any{"name": "fan", "maxConcurrentBranches": 2, "join": "join", "failurePolicy": "continue_on_error", "branches": []any{map[string]any{"name": "left", "start": "plan"}, map[string]any{"name": "right", "start": "right"}}}}
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// Both generated commands must reach this barrier before either can finish.
// It is test synchronization only; production custody proves stopped writers.
func parallelQualificationBarrier(t *testing.T, repository string) <-chan struct{} {
	t.Helper()
	ready := make(chan struct{})
	var mu sync.Mutex
	active := map[string]int{}
	paired := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		branch := req.URL.Query().Get("branch")
		if branch != "left" && branch != "right" {
			http.Error(w, "unknown branch", http.StatusBadRequest)
			return
		}
		mu.Lock()
		if req.Context().Err() != nil {
			mu.Unlock()
			return
		}
		active[branch]++
		if !paired && active["left"] > 0 && active["right"] > 0 {
			paired = true
			close(ready)
		}
		mu.Unlock()
		defer func() { mu.Lock(); active[branch]--; mu.Unlock() }()
		select {
		case <-ready:
			w.WriteHeader(http.StatusNoContent)
		case <-req.Context().Done():
		case <-t.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	endpoint := "http://host.docker.internal:" + strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port) + "/started"
	if err := os.WriteFile(filepath.Join(repository, "qualification-notify"), []byte(endpoint), 0600); err != nil {
		t.Fatal(err)
	}
	return ready
}

func assertParallelQualificationChildren(t *testing.T, children []triggerqueue.ChildRecord, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	default:
		t.Fatal("generated siblings never overlapped")
	}
	if len(children) != 2 || children[0].RunID == children[1].RunID || children[0].Identity.StageOccurrence == children[1].Identity.StageOccurrence {
		t.Fatal("parallel children lost separate stage ownership", children)
	}
	for _, child := range children {
		if child.Sequence != 1 || child.Identity.InvocationKey != "qualification-child" {
			t.Fatal("stage-local invocation key was not independent", child)
		}
	}
}

// Diagnostics retain only route, status and duration, never headers or bodies.
func parallelQualificationHTTPTrace(t *testing.T, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		out := &parallelQualificationResponse{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(out, r)
		if elapsed := time.Since(started); elapsed >= 500*time.Millisecond || out.status >= 400 && out.status != http.StatusNotFound {
			t.Logf("qualification HTTP %s status=%d duration=%s", r.URL.Path, out.status, elapsed)
		}
	})
}

type parallelQualificationResponse struct {
	http.ResponseWriter
	status int
}

func (w *parallelQualificationResponse) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *parallelQualificationResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
