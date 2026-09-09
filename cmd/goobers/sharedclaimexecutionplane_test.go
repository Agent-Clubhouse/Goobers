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
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/sharedclaim"
)

func TestDispatchExecSharedDeadlineStopsActualStageProcess(t *testing.T) {
	now := time.Now()
	entry := httpapi.ClaimEntry{Gaggle: "example", Provider: "github", ExternalID: "42", RunID: "shared-run", ClaimedAt: now,
		ExpiresAt: now.Add(time.Second), SharedDeadline: now.Add(time.Second), SharedOwner: sharedclaim.Owner{Instance: "instance", Run: "shared-run", Token: "owner"}}
	surrendered := make(chan dispatcher.SurrenderedResult, 1)
	var startLease sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == apicontract.ClaimListPath {
			startLease.Do(func() {
				entry.ExpiresAt = time.Now().Add(time.Second)
				entry.SharedDeadline = entry.ExpiresAt
			})
			_ = json.NewEncoder(w).Encode(httpapi.ClaimListResponse{ClaimVisibility: "shared", ObservedAt: time.Now(), Entries: []httpapi.ClaimEntry{entry}})
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/surrender") {
			var result dispatcher.SurrenderedResult
			if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
				t.Error(err)
			}
			surrendered <- result
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	t.Setenv(dispatcher.EnvRunID, entry.RunID)
	t.Setenv(dispatcher.EnvGaggle, entry.Gaggle)
	t.Setenv(dispatcher.EnvStage, "long-task")
	t.Setenv(dispatcher.EnvAttempt, "1")
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvPodToken, "parent-only")
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","sleep 10; echo executed-after-expiry"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "20s")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if code := runDispatchExecContext(ctx, &stdout, &stderr); code != 0 {
		t.Fatalf("failed to surrender lease loss: code=%d stderr=%s", code, stderr.String())
	}
	select {
	case result := <-surrendered:
		if result.Result.Status != apiv1.ResultFailure || result.Result.Error == nil || result.Result.Error.Code != "execution_authority_ended" || result.Verdict != nil {
			t.Fatalf("lease loss became a successful result: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("lease loss was not surrendered")
	}
	if stdout.Len() != 0 || ctx.Err() != nil {
		t.Fatalf("process outlived lease fencing: stdout=%s ctx=%v", stdout.String(), ctx.Err())
	}
}

func TestRemoteExecutionPolicyUsesDaemonPinAndRefusesTerminalRun(t *testing.T) {
	for _, mode := range []string{"local", "shared"} {
		for _, token := range []string{"parent-only", ""} {
			t.Run(mode+"/"+token, func(t *testing.T) {
				layout, run := newPinnedClaimResolverRun(t, mode)
				service := newDaemonClaimService(layout, nil, nil)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					wantAuthorization := ""
					if token != "" {
						wantAuthorization = "Bearer " + token
					}
					if r.URL.Path != apicontract.ClaimListPath || r.Header.Get("Authorization") != wantAuthorization {
						http.Error(w, "unexpected authority or route", http.StatusForbidden)
						return
					}
					var request httpapi.ClaimListRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RunID != "shared-run" || !request.Execution || !request.IncludeHistory || request.Scope != httpapi.ClaimListScopeRun {
						http.Error(w, "unexpected execution query", http.StatusBadRequest)
						return
					}
					response, err := service.List(r.Context(), request)
					if err != nil {
						http.Error(w, err.Error(), http.StatusConflict)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(response)
				}))
				t.Cleanup(server.Close)
				fence := remoteSharedExecutionFence(server.URL, func(runID string) (string, error) {
					return workerExecutionBearer(&livejournal.HTTPEmitter{BaseURL: server.URL, Token: token}, runID)
				})
				ctx, stop, err := fence(t.Context(), apiv1.InvocationEnvelope{RunID: "shared-run"})
				if err != nil {
					t.Fatal(err)
				}
				if (ctx == t.Context()) != (mode == "local") {
					t.Fatal("remote policy did not select the expected fence")
				}
				stop()
				if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
					t.Fatal(err)
				}
				_, stop, err = fence(t.Context(), apiv1.InvocationEnvelope{RunID: "shared-run"})
				stop()
				if (err != nil) != (mode == "shared") {
					t.Fatalf("terminal execution policy: %v", err)
				}
			})
		}
	}
}

func TestWorkerExecutionWithoutBearerRefusesRemoteDaemon(t *testing.T) {
	emitter := &livejournal.HTTPEmitter{BaseURL: "https://daemon.example"}
	fence := remoteSharedExecutionFence(emitter.BaseURL, func(runID string) (string, error) { return workerExecutionBearer(emitter, runID) })
	_, stop, err := fence(t.Context(), apiv1.InvocationEnvelope{RunID: "run"})
	stop()
	if err == nil {
		t.Fatal("remote execution bypassed missing credentials")
	}
}

func TestExecutionSnapshotRefusesNamespaceOrMissingHistory(t *testing.T) {
	layout, _ := newPinnedClaimResolverRun(t, "shared")
	service := newDaemonClaimService(layout, nil, nil)
	for _, request := range []httpapi.ClaimListRequest{
		{RunID: "shared-run", Scope: httpapi.ClaimListScopeRun, Execution: true},
		{RunID: "shared-run", Gaggle: "example", Provider: "github", Scope: httpapi.ClaimListScopeNamespace, Execution: true, IncludeHistory: true},
	} {
		if _, err := service.List(t.Context(), request); err == nil {
			t.Fatalf("uncontained execution snapshot: %+v", request)
		}
	}
}
