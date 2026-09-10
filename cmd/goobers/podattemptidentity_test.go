package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestRepassedPodArtifactsRetainNewContentAndLogicalAttempt(t *testing.T) {
	for _, tc := range []struct{ name, logical, class string }{
		{"repass", "1", ""}, {"infra-retry", "2", "infra"}, {"policy-retry", "2", "policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			run, err := journal.Create(root, journal.RunIdentity{RunID: "repass", Gaggle: "test", Workflow: "workflow", WorkflowVersion: 1, Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			writer, err := livejournal.NewWriter(func(g string) (string, bool) { return root, g == "test" })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(writer.Close)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer pod-token" {
					t.Error("journal token changed")
					w.WriteHeader(403)
					return
				}
				var req livejournal.EmitRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				out, err := writer.Emit(r.Context(), req)
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				if err := json.NewEncoder(w).Encode(out); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			for _, key := range dispatcher.DispatcherControlEnv {
				t.Setenv(key, "")
			}
			t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
			t.Setenv(dispatcher.EnvPodToken, "pod-token")
			t.Setenv(dispatcher.EnvRunID, "repass")
			t.Setenv(dispatcher.EnvGaggle, "test")
			t.Setenv(dispatcher.EnvStage, "curate")
			t.Setenv(dispatcher.EnvAttempt, "1")
			var stderr strings.Builder
			for _, physical := range []string{"1", "2"} {
				t.Setenv(dispatcher.EnvPodAttempt, physical)
				if physical == "2" {
					t.Setenv(dispatcher.EnvAttempt, tc.logical)
					t.Setenv(dispatcher.EnvAttemptClass, tc.class)
				}
				logical, _ := strconv.Atoi(os.Getenv(dispatcher.EnvAttempt))
				refs := recordStageArtifacts(context.Background(), &stderr, map[string][]byte{"result": []byte("revision-" + physical)})
				if len(refs) != 1 {
					t.Fatalf("artifact refs=%v (%s)", refs, stderr.String())
				}
				if err := (podArtifactRecorder{stderr: &stderr}).Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "curate", Attempt: logical, Runner: map[string]any{"revision": physical}}); err != nil {
					t.Fatal(err)
				}
				id, ok := podStageIdentityFromEnv()
				if !ok {
					t.Fatal("missing heartbeat identity")
				}
				emitPodStageHeartbeat(context.Background(), &stderr, id, &livejournal.HTTPEmitter{BaseURL: server.URL, Token: "pod-token"}, 1)
			}
			if stderr.Len() != 0 {
				t.Fatal(stderr.String())
			}
			reader, err := journal.OpenRead(filepath.Join(root, "repass"))
			if err != nil {
				t.Fatal(err)
			}
			records, err := reader.EventRecords()
			if err != nil {
				t.Fatal(err)
			}
			var contents []string
			annotations, heartbeats := 0, 0
			for _, record := range records {
				e := record.Event
				switch e.Type {
				case journal.EventArtifactRecorded:
					wantAttempt, wantClass := 1, ""
					if len(contents) > 0 {
						wantAttempt, _ = strconv.Atoi(tc.logical)
						wantClass = tc.class
					}
					if e.Attempt != wantAttempt || string(e.AttemptClass) != wantClass {
						t.Fatalf("artifact lineage changed: %+v", e)
					}
					data, err := os.ReadFile(filepath.Join(root, "repass", e.Ref.Path))
					if err != nil {
						t.Fatal(err)
					}
					contents = append(contents, string(data))
				case journal.EventRunnerAnnotation:
					annotations++
				case journal.EventStageHeartbeat:
					heartbeats++
				}
			}
			if strings.Join(contents, ",") != "revision-1,revision-2" || annotations != 2 || heartbeats != 2 {
				t.Fatalf("deduped later pod: artifacts=%v annotations=%d heartbeats=%d", contents, annotations, heartbeats)
			}
		})
	}
}

func TestDispatchExecUsesPhysicalSurrenderOrdinal(t *testing.T) {
	var path, token string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLocalExecutionPolicy(w, r) {
			return
		}
		path = r.URL.Path
		token = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	for _, key := range dispatcher.DispatcherControlEnv {
		t.Setenv(key, "")
	}
	t.Setenv(dispatcher.EnvRunID, "run-1")
	t.Setenv(dispatcher.EnvStage, "probe")
	t.Setenv(dispatcher.EnvAttempt, "1")
	t.Setenv(dispatcher.EnvPodAttempt, "7")
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","echo new-result"]`)
	t.Setenv(dispatcher.EnvStageTimeout, "10s")
	var stderr strings.Builder
	if code := runDispatchExecContext(context.Background(), io.Discard, &stderr); code != 0 {
		t.Fatalf("exit=%d: %s", code, stderr.String())
	}
	if path != "/api/v1/runs/run-1/stages/probe/attempts/7/surrender" || token != "Bearer pod-token" {
		t.Fatalf("physical surrender=%s token=%s", path, token)
	}
	t.Setenv(dispatcher.EnvPodAttempt, "invalid")
	if code := runDispatchExecContext(context.Background(), io.Discard, &stderr); code == 0 {
		t.Fatal("invalid physical identity accepted")
	}
}
