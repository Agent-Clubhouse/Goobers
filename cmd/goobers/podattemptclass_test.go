package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// Cross the actual pod-rendering, executor, HTTP, writer and disk boundaries.
// The pod-loss RC probe exposed result/stdout records losing the retry class
// even while engine-authored lifecycle records retained it.
func TestPodArtifactsPreserveDispatchedAttemptClass(t *testing.T) {
	for _, template := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			class   journal.AttemptClass
			attempt int
			legacy  bool
		}{
			{name: "initial", attempt: 1},
			{name: "policy", class: journal.AttemptPolicy, attempt: 2},
			{name: "infra", class: journal.AttemptInfra, attempt: 2},
			{name: "human", class: journal.AttemptHuman, attempt: 2},
			{name: "legacy", attempt: 2, legacy: true},
		} {
			name := "image/" + tc.name
			if template {
				name = "template/" + tc.name
			}
			t.Run(name, func(t *testing.T) {
				runsDir := t.TempDir()
				run, err := journal.Create(runsDir, journal.RunIdentity{
					RunID: "lineage", Gaggle: "test", Workflow: "workflow", WorkflowVersion: 1,
					Trigger: journal.Trigger{Kind: journal.TriggerManual},
				}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := run.Close(); err != nil {
					t.Fatal(err)
				}
				writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) { return runsDir, gaggle == "test" })
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(writer.Close)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req livejournal.EmitRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					response, err := writer.Emit(r.Context(), req)
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					if err := json.NewEncoder(w).Encode(response); err != nil {
						t.Error(err)
					}
				}))
				t.Cleanup(server.Close)
				cfg := dispatcher.Config{Namespace: "test", WriteAPIBase: server.URL}
				attempt := dispatcher.Attempt{RunID: "lineage", Gaggle: "test", Workflow: "workflow", Stage: "curate", Number: tc.attempt, Class: tc.class, CLIStage: true}
				runner := dispatcher.RunnerSpec{Name: "linux", OS: "linux", Host: "runner:test", Restrictions: []string{"env:default-deny"}}
				var pod *corev1.Pod
				if template {
					pod, err = dispatcher.RenderFromTemplate(cfg, attempt, runner, &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
						Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "stage", Image: "runner:test"}}}},
					}})
				} else {
					pod, err = dispatcher.RenderPod(cfg, attempt, runner)
				}
				if err != nil {
					t.Fatal(err)
				}
				// Isolate ambient plane settings; all identity comes from the pod.
				for _, key := range dispatcher.DispatcherControlEnv {
					t.Setenv(key, "")
				}
				stamped := false
				for _, env := range pod.Spec.Containers[0].Env {
					t.Setenv(env.Name, env.Value)
					if env.Name == dispatcher.EnvAttemptClass {
						stamped = true
					}
				}
				if !stamped {
					t.Fatal("pod did not stamp attempt class, including the explicit initial default")
				}
				if tc.legacy {
					if err := os.Unsetenv(dispatcher.EnvAttemptClass); err != nil {
						t.Fatal(err)
					}
				} else if !slices.Contains(stageEnvironment(), dispatcher.EnvAttemptClass+"="+string(tc.class)) {
					t.Fatal("CLI default-deny environment lost attempt class")
				}
				var stderr strings.Builder
				pointers := recordStageArtifactsTyped(context.Background(), &stderr, map[string][]byte{
					"result": []byte(`{"status":"success"}`), "stdout.log": []byte("completed\n"),
				}, map[string]string{"result": "application/json"})
				if len(pointers) != 2 {
					t.Fatalf("artifact pointers = %d", len(pointers))
				}
				if _, err := (podArtifactRecorder{stderr: &stderr}).RecordArtifact("agent-result", []byte("agent completed")); err != nil {
					t.Fatal(err)
				}
				if stderr.Len() != 0 {
					t.Fatal(stderr.String())
				}
				rd, err := journal.OpenRead(filepath.Join(runsDir, "lineage"))
				if err != nil {
					t.Fatal(err)
				}
				records, err := rd.EventRecords()
				if err != nil {
					t.Fatal(err)
				}
				var artifacts []journal.Event
				for _, record := range records {
					event := record.Event
					if event.Type != journal.EventArtifactRecorded {
						continue
					}
					if event.Stage != "curate" || event.Attempt != tc.attempt || event.AttemptClass != tc.class {
						t.Fatalf("persisted %s lineage = %s/%d/%q; want curate/%d/%q", event.Name, event.Stage, event.Attempt, event.AttemptClass, tc.attempt, tc.class)
					}
					artifacts = append(artifacts, event)
				}
				if len(artifacts) != 3 {
					t.Fatalf("persisted artifacts = %d", len(artifacts))
				}
				wantNormative := 3
				if tc.class == journal.AttemptInfra {
					wantNormative = 0
				}
				if got := len(journal.ConformanceView(artifacts)); got != wantNormative {
					t.Fatalf("normative artifacts = %d; want %d", got, wantNormative)
				}
				t.Setenv(dispatcher.EnvStageIsCLI, "false")
				for _, kv := range stageEnvironment() {
					if strings.HasPrefix(kv, dispatcher.EnvAttemptClass+"=") {
						t.Fatal("non-CLI subprocess inherited attempt class")
					}
				}
			})
		}
	}
}

func TestPodArtifactFailureDiagnosticRetainsAttemptLineage(t *testing.T) {
	journalPlane, emittedOps := recordingJournalPlane(t)
	blobPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(blobPlane.Close)
	t.Setenv(dispatcher.EnvDaemonAPI, journalPlane)
	t.Setenv(dispatcher.EnvBlobEndpoint, blobPlane.URL)
	t.Setenv(dispatcher.EnvPodToken, "test-token")
	t.Setenv(dispatcher.EnvRunID, "lineage-diagnostic")
	t.Setenv(dispatcher.EnvStage, "curate")
	t.Setenv(dispatcher.EnvAttempt, "2")
	t.Setenv(dispatcher.EnvAttemptClass, string(journal.AttemptInfra))
	var stderr strings.Builder
	recordStageArtifacts(context.Background(), &stderr, map[string][]byte{"result": []byte("done")})
	var names []string
	for _, op := range emittedOps() {
		if op.Artifact == nil {
			continue
		}
		artifact := op.Artifact
		if artifact.Attempt != 2 || artifact.Class != journal.AttemptInfra {
			t.Fatalf("artifact %q lineage = %d/%q", artifact.Name, artifact.Attempt, artifact.Class)
		}
		names = append(names, artifact.Name)
	}
	if !slices.Contains(names, "curate/"+blobWriteThroughFailureArtifact) {
		t.Fatalf("missing durable blob failure diagnostic: %v (%s)", names, stderr.String())
	}
}
