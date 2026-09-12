package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func configurePreparedPodPublication(t *testing.T, endpoint string) {
	t.Helper()
	for _, key := range dispatcher.DispatcherControlEnv {
		t.Setenv(key, "")
	}
	for key, value := range map[string]string{
		dispatcher.EnvDaemonAPI: endpoint, dispatcher.EnvBlobEndpoint: endpoint,
		dispatcher.EnvPodToken: "prepared-pod-token", dispatcher.EnvRunID: "prepared-run",
		dispatcher.EnvGaggle: "test", dispatcher.EnvStage: "curate",
		dispatcher.EnvAttempt: "2", dispatcher.EnvAttemptClass: "infra", dispatcher.EnvPodAttempt: "7",
	} {
		t.Setenv(key, value)
	}
}

func TestPodPreparedArtifactsPublishExactBlobsBeforeSmallReferenceEmits(t *testing.T) {
	var mu sync.Mutex
	blobs := map[string][]byte{}
	var ops []livejournal.Op
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer prepared-pod-token" {
			t.Error("publication did not use the pod token")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPut {
			digest := strings.TrimPrefix(r.URL.Path, dispatcher.BlobPathPrefix)
			if digest != journal.Digest(data) {
				t.Error("PUT changed the prepared content address")
			}
			blobs[digest] = data
			w.WriteHeader(http.StatusCreated)
			return
		}
		if len(data) > 2048 {
			t.Errorf("journal request embeds payload: %d bytes", len(data))
		}
		var req livejournal.EmitRequest
		if err := json.Unmarshal(data, &req); err != nil || len(req.Ops) != 1 {
			t.Errorf("reference emit: %v %+v", err, req)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		op := req.Ops[0]
		if op.Artifact == nil || op.Artifact.Ref == nil || len(op.Artifact.Data) != 0 {
			t.Error("prepared publication did not use reference adoption")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, exists := blobs[op.Artifact.Ref.Digest]; !exists {
			t.Error("journal emit preceded blob publication")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ops = append(ops, op)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	configurePreparedPodPublication(t, server.URL)
	recorder := podArtifactRecorder{stderr: io.Discard}
	contents := [][]byte{{}, bytes.Repeat([]byte("large-evidence\n"), 400000), []byte("member"), []byte("index"), []byte("member")}
	for _, data := range contents {
		ref, err := recorder.RecordPreparedArtifact(context.Background(), "task/artifact-set.json", "text/plain", data)
		if err != nil || ref.Digest != journal.Digest(data) || ref.Size != int64(len(data)) || ref.MediaType != "text/plain" {
			t.Fatalf("publication ref=%+v err=%v", ref, err)
		}
		mu.Lock()
		stored, exists := blobs[ref.Digest]
		if !exists || !bytes.Equal(stored, data) {
			t.Error("prepared bytes were lost or changed")
		}
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ops) != len(contents) || ops[2].Key == ops[3].Key || ops[2].Key != ops[4].Key {
		t.Fatalf("semantic member/index collision or unstable redelivery key: %+v", ops)
	}
	for _, op := range ops {
		if !strings.HasPrefix(op.Key, "pod/7/") || op.Artifact.Attempt != 2 || op.Artifact.Class != journal.AttemptInfra || op.Artifact.Name != "curate/task/artifact-set.json" {
			t.Fatalf("physical identity or logical lineage changed: %+v", op)
		}
	}
}

func TestPodPreparedArtifactRefusesEitherPublicationFailure(t *testing.T) {
	for _, fail := range []string{"blob", "journal"} {
		t.Run(fail, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				step := "journal"
				if r.Method == http.MethodPut {
					step = "blob"
				}
				calls = append(calls, step)
				if step == fail {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusCreated)
			}))
			t.Cleanup(server.Close)
			configurePreparedPodPublication(t, server.URL)
			ref, err := (podArtifactRecorder{stderr: io.Discard}).RecordPreparedArtifact(context.Background(), "evidence", "text/plain", nil)
			if err == nil || ref != (journal.Ref{}) {
				t.Fatalf("failed %s returned usable pointer: %+v err=%v", fail, ref, err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := "blob"
			if fail == "journal" {
				want = "blob,journal"
			}
			if strings.Join(calls, ",") != want {
				t.Fatalf("publication order=%v, want %s", calls, want)
			}
		})
	}
}

func TestPodPreparedArtifactRequiresConfiguredPlanesAndIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("publication started before identity and plane validation")
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	for _, missing := range []string{dispatcher.EnvDaemonAPI, dispatcher.EnvBlobEndpoint, dispatcher.EnvRunID, dispatcher.EnvGaggle, dispatcher.EnvStage, dispatcher.EnvAttempt} {
		configurePreparedPodPublication(t, server.URL)
		t.Setenv(missing, "")
		ref, err := (podArtifactRecorder{}).RecordPreparedArtifact(context.Background(), "evidence", "text/plain", nil)
		if err == nil || ref != (journal.Ref{}) {
			t.Fatalf("missing %s accepted: %+v err=%v", missing, ref, err)
		}
	}
}
