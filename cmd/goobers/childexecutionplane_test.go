package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/sharedclaim"
)

type childObservationFunc func(context.Context, httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error)

func (f childObservationFunc) List(ctx context.Context, req httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
	return f(ctx, req)
}

func childExecutionHTTPFixture(t *testing.T, observe httpapi.ChildExecutionObserver) (childKitFixture, childpod.Contract, string, *httptest.Server) {
	t.Helper()
	f := newChildKitFixture(t, true)
	s, id := f.writer.service, f.writer.identity
	launcher := &queuedChildLauncher{layout: s.layout, queue: s.childQueue, authority: s.children}
	s.childCredentials = launcher.credentialCeiling
	writer := f.writer.recorder.(*journal.Run)
	if err := writer.Append(journal.Event{Type: journal.EventStageStarted, Stage: "check", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenReadOnly(writer.Dir())
	if err != nil {
		t.Fatal(err)
	}
	event, err := childPodStarted(rd, "check", 1, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := childpod.Contract{Version: 1, Identity: id, Stage: "check", Attempt: 1, PodAttempt: int(event.Seq), StartedAt: event.Time, Ceiling: credentials.NewChildCeiling(false, []string{"agent:model"}, []string{"agent:model"})}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	digest := journal.Digest(raw)
	blobs := childpod.ScopedBlobs{Queue: s.childQueue, Identity: f.child.Identity}
	if err = blobs.Put(t.Context(), digest, raw); err != nil {
		t.Fatal(err)
	}
	custody := &childInvocationBlobs{ScopedBlobs: blobs, recorder: writer}
	retained := childpod.RetainedAttempt{Version: 1, Input: engine.ChildDispatchInput{Attempt: dispatcher.Attempt{RunID: id.RunID, Gaggle: id.Gaggle, Workflow: id.Workflow, Stage: "check", Number: 1, PodAttempt: c.PodAttempt, ChildExecutionDigest: digest}, Queue: "worker", Eligible: []dispatcher.RunnerSpec{{OS: "linux", HostKind: instance.RunnerHostImage}}}}
	if err = custody.keepAttempt(t.Context(), retained); err != nil {
		t.Fatal(err)
	}
	if err = custody.BindContract(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	key, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := key.MintChildPod(id.RunID, digest, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithGeneratedChildExecutionObserver(childExecutionPlane{service: s, claims: observe}))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return f, c, token, server
}

func TestChildExecutionObserverUsesParentLeaseAndStopsOnCancellation(t *testing.T) {
	for _, scenario := range []string{"local-cancel", "shared-expiry", "shared-revoked", "stalled-read", "foreign-owner", "finished-attempt"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			var revoked atomic.Bool
			var expires time.Time
			observe := childObservationFunc(func(ctx context.Context, req httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
				calls.Add(1)
				if req.Scope != httpapi.ClaimListScopeRun || !req.Execution || !req.IncludeHistory || req.Gaggle != "" {
					return httpapi.ClaimListResponse{}, context.Canceled
				}
				if scenario == "stalled-read" && calls.Load() > 1 {
					<-ctx.Done()
					return httpapi.ClaimListResponse{}, ctx.Err()
				}
				mode := "local"
				var entries []httpapi.ClaimEntry
				if strings.HasPrefix(scenario, "shared") || scenario == "foreign-owner" {
					mode = "shared"
					if expires.IsZero() {
						expires = time.Now().Add(1500 * time.Millisecond)
					}
					runID := req.RunID
					if scenario == "foreign-owner" {
						runID = "foreign"
					}
					entries = []httpapi.ClaimEntry{{RunID: runID, Gaggle: "example", Provider: "github", ExternalID: "42", ExpiresAt: expires, SharedDeadline: expires, SharedRevoked: revoked.Load(), SharedOwner: sharedclaim.Owner{Run: runID, Instance: "instance", Token: "lease"}}}
				}
				return httpapi.ClaimListResponse{ClaimVisibility: mode, ObservedAt: time.Now(), Entries: entries}, nil
			})
			f, contract, token, server := childExecutionHTTPFixture(t, observe)
			ctx, stop, err := remoteChildExecutionFence(t.Context(), server.URL, token, contract)
			defer stop()
			if scenario == "foreign-owner" {
				if err == nil {
					t.Fatal("foreign claim owner admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "local-cancel" {
				if err := f.writer.service.childQueue.FenceChildParent(t.Context(), f.child.Identity.ChildParent, "human", time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "shared-revoked" {
				revoked.Store(true)
			}
			if scenario == "finished-attempt" {
				if err := f.writer.recorder.(*journal.Run).Append(journal.Event{Type: journal.EventStageFinished, Stage: "check", Attempt: 1, Status: "completed"}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-ctx.Done():
				if context.Cause(ctx) == nil {
					t.Fatal("authority loss had no cause")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("child continued after execution authority ended")
			}
			if scenario == "shared-expiry" && calls.Load() < 2 {
				t.Fatal("shared authority was not observed repeatedly")
			}
		})
	}
}

func TestChildExecutionObserverRequestsChildAndReceivesOriginalParentIdentity(t *testing.T) {
	var parent string
	observe := childObservationFunc(func(_ context.Context, req httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
		parent = req.RunID
		return httpapi.ClaimListResponse{ClaimVisibility: "shared", ObservedAt: time.Now(), Entries: []httpapi.ClaimEntry{{RunID: req.RunID, ExpiresAt: time.Now().Add(time.Minute), SharedDeadline: time.Now().Add(time.Minute)}}}, nil
	})
	_, c, token, server := childExecutionHTTPFixture(t, observe)
	client, err := claimsclient.NewExecutionObserver(claimsclient.HTTPConfig{BaseURL: server.URL, Token: token, RunID: c.Identity.RunID})
	if err != nil {
		t.Fatal(err)
	}
	mode, listing, err := client.ExecutionSnapshot(t.Context())
	if err != nil || mode != "shared" || len(listing.Entries) != 1 || parent != c.Identity.Child.ParentRunID || listing.Entries[0].RunID != parent {
		t.Fatal(mode, listing, parent, err)
	}
}
