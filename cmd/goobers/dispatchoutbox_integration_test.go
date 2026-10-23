package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
)

func TestRunDispatchExecSurrendersV3OutboxManifest(t *testing.T) {
	var surrendered dispatcher.SurrenderedResult
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLocalExecutionPolicy(w, r) {
			return
		}
		if strings.HasPrefix(r.URL.Path, dispatcher.BlobPathPrefix) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusCreated)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/surrender") {
			data, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(data, &surrendered); err != nil {
				t.Error(err)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	t.Chdir(t.TempDir())
	t.Setenv(dispatcher.EnvRunID, "outbox-run")
	t.Setenv(dispatcher.EnvStage, "build")
	t.Setenv(dispatcher.EnvAttempt, "1")
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvBlobEndpoint, server.URL)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvStageOutbox, `["report.txt"]`)
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","printf report > report.txt"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")

	if code := runDispatchExecContext(context.Background(), io.Discard, io.Discard); code != 0 {
		t.Fatalf("exit code=%d", code)
	}
	if surrendered.ContractVersion != dispatcher.SurrenderContractV3 ||
		!surrendered.OutboxExported || !surrendered.OutboxExecutionStarted ||
		len(surrendered.Outbox) != 1 ||
		surrendered.Outbox[0].RelPath != "report.txt" {
		t.Fatalf("surrendered=%+v", surrendered)
	}
	if _, err := os.Stat("report.txt"); err != nil {
		t.Fatal(err)
	}
}
