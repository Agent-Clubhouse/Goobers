package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestParentCredentialsRequireActiveAttemptAndCurrentModelPermission(t *testing.T) {
	for _, mode := range []string{"stage-finished-during-resolution", "joined", "policy-disabled"} {
		t.Run(mode, func(t *testing.T) {
			f := newPinnedChildFixture(t, func(root string) {
				path := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				// The parent still needs its own model even when it cannot delegate model access.
				data = bytes.ReplaceAll(data, []byte("allowedCapabilities: [agent:model, repo:push]"), []byte("allowedCapabilities: [repo:push]"))
				if err = os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			})
			run, env := configuredChildStage(t, f)
			rd, err := journal.OpenReadOnly(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			id, err := rd.Identity()
			if err != nil {
				t.Fatal(err)
			}
			started, err := childPodStarted(rd, "plan", 1, false)
			if err != nil {
				t.Fatal(err)
			}
			c := childpod.Contract{Version: 1, Identity: id, ParentOrigin: env.ChildWorkflowOrigin, Stage: "plan", Attempt: 1, PodAttempt: int(started.Seq), StartedAt: started.Time, Ceiling: credentials.NewChildCeiling(false, []string{"agent:model"}, []string{"agent:model"})}
			data, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			digest := journal.Digest(data)
			store := childpod.ParentBlobs{RunDir: run.Dir(), Identity: id}
			if err = store.Put(t.Context(), digest, data); err != nil {
				t.Fatal(err)
			}
			if err = store.BindContract(t.Context(), digest); err != nil {
				t.Fatal(err)
			}
			mark := func(kind string) {
				t.Helper()
				if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1, Runner: map[string]any{"kind": kind, "contractDigest": digest}}); err != nil {
					t.Fatal(err)
				}
			}
			queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = queue.Close() })
			shared := journal.NewRegistryScrubber()
			s := newDaemonCredentialService(f.layout, f.cfg, nil, shared, nil).withStageGrants(f.layout.Root, "127.0.0.1:8080", false)
			t.Cleanup(func() { unregisterDaemonStageGrants(f.layout.Root, s) })
			if err = s.enableChildWorkflows(queue, f.applied); err != nil {
				t.Fatal(err)
			}
			s.Replace(credentialPlaneDefinitionsFromSet(f.applied))
			s.log, _, err = journal.OpenInstanceLog(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.log.Close() })
			key, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
			if err != nil {
				t.Fatal(err)
			}
			auth, err := podauth.NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
			if err != nil {
				t.Fatal(err)
			}
			token, err := key.MintWorkflowParentPod(id.RunID, digest, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			resolved := 0
			var duringResolve func()
			s.buildSources = func(credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error) {
				resolver, err := credentials.NewResolverWithExpiring(nil, nil, map[string]credentials.ResolveFunc{"agent:model": func(context.Context) (string, error) {
					resolved++
					if duringResolve != nil {
						duringResolve()
					}
					return "parent-model-secret", nil
				}}, nil)
				return resolver, []credentials.Grant{{Capability: "agent:model", Ref: "agent:model"}}, err
			}
			handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithCredentialService(s), httpapi.WithWorkflowParentCredentialService(parentCredentialPlane{service: s}), httpapi.WithWorkflowParentExecutionObserver(parentExecutionPlane{service: s, claims: childObservationFunc(func(ctx context.Context, req httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
				if req.RunID != id.RunID || !req.Execution || !req.IncludeHistory || req.Scope != httpapi.ClaimListScopeRun {
					t.Error("parent observed another lease owner")
				}
				return httpapi.ClaimListResponse{ClaimVisibility: "local", ObservedAt: time.Now()}, nil
			})}))
			if err != nil {
				t.Fatal(err)
			}
			request := func(input httpapi.CredentialResolveRequest) *httptest.ResponseRecorder {
				data, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest(http.MethodPost, apicontract.CredentialResolvePath, bytes.NewReader(data))
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w
			}
			input := httpapi.CredentialResolveRequest{RunID: id.RunID, Stage: "plan", Attempt: 1}
			if w := request(input); w.Code != http.StatusForbidden || resolved != 0 {
				t.Fatal("unowned parent minted", w.Code, w.Body, resolved)
			}
			mark(childpod.ParentWriterStarted)
			for _, bad := range []httpapi.CredentialResolveRequest{{RunID: id.RunID, Stage: "other"}, {RunID: id.RunID, Stage: "plan", Attempt: 2}, {RunID: id.RunID, Stage: "plan", Grant: true}, {RunID: id.RunID, Stage: "plan", Capabilities: []string{"repo:push"}}} {
				if w := request(bad); w.Code != http.StatusForbidden || resolved != 0 {
					t.Fatal("broader authority", w.Code, w.Body, resolved)
				}
			}
			if w := request(input); w.Code != http.StatusOK || resolved != 1 || !strings.Contains(w.Body.String(), "parent-model-secret") || w.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatal("parent model unavailable", w.Code, w.Body, resolved)
			}
			if bytes.Contains(shared.Scrub([]byte("parent-model-secret")), []byte("parent-model-secret")) {
				t.Fatal("credential escaped scrubber registration")
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			owned, stop, err := remoteChildExecutionFence(t.Context(), server.URL, token, c)
			defer stop()
			if err != nil {
				t.Fatal("live parent worker fence refused", err)
			}
			wantResolved := 1
			switch mode {
			case "stage-finished-during-resolution":
				duringResolve = func() {
					if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1}); err != nil {
						t.Fatal(err)
					}
				}
				wantResolved = 2
			case "joined":
				mark(childpod.ParentWriterJoined)
			case "policy-disabled":
				next := *f.applied
				next.Workflows = append(next.Workflows[:0:0], f.applied.Workflows...)
				disabled := false
				next.Workflows[0] = *next.Workflows[0].DeepCopy()
				next.Workflows[0].Spec.Enabled = &disabled
				if err := s.children.ApplyDefinitions(t.Context(), &next, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if w := request(input); w.Code != http.StatusForbidden || resolved != wantResolved || strings.Contains(w.Body.String(), "parent-model-secret") {
				t.Fatal("revoked attempt returned secret", w.Code, w.Body, resolved)
			}
			select {
			case <-owned.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("revoked parent worker retained execution")
			}
		})
	}
}
