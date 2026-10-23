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

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestCIPollOutboxUsesResolvedCredentialScrubber(t *testing.T) {
	t.Chdir(t.TempDir())
	setPodCIPollEnv(t, defaultCIPollFixture())
	t.Setenv(dispatcher.EnvStageCommand, `["goobers","ci-poll"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvAgenticKitDigest, "")
	t.Setenv(dispatcher.EnvStageOutbox, `["report.txt"]`)
	var token string
	previous := newPRPoller
	newPRPoller = func(value string) executor.PRPoller {
		token = value
		if err := os.WriteFile("report.txt", []byte("diagnostic payload: "+value), 0o600); err != nil {
			t.Fatal(err)
		}

		return &recordingPRPoller{result: providers.PullRequestPollResult{CheckState: providers.CheckStatePassing}}
	}
	t.Cleanup(func() { newPRPoller = previous })
	uploaded := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		uploaded <- string(data)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	t.Setenv(dispatcher.EnvBlobEndpoint, server.URL)
	out := runStage(context.Background(), io.Discard, io.Discard)
	if out.Result.Status != apiv1.ResultSuccess || token == "" || !out.OutboxEligible || out.OutboxScrubber == nil {
		t.Fatalf("bad fixture: %+v token=%q", out, token)
	}
	files, err := publishPodOutbox(t.Context(), out.OutboxScrubber)
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	if got := <-uploaded; strings.Contains(got, token) {
		t.Fatalf("credential-aware scrubber leaked token in %q", got)
	}
}

func TestNonzeroCommandStillExportsOutbox(t *testing.T) {
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
	t.Setenv(dispatcher.EnvRunID, "outbox-failure")
	t.Setenv(dispatcher.EnvStage, "build")
	t.Setenv(dispatcher.EnvAttempt, "1")
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvBlobEndpoint, server.URL)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvStageOutbox, `["report.txt"]`)
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","printf report > report.txt; exit 7"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")
	_ = runDispatchExecContext(context.Background(), io.Discard, io.Discard)
	if surrendered.ContractVersion != dispatcher.SurrenderContractV3 ||
		!surrendered.OutboxExported || len(surrendered.Outbox) != 1 ||
		surrendered.Outbox[0].RelPath != "report.txt" {
		t.Fatalf("surrendered=%+v", surrendered)
	}
}
