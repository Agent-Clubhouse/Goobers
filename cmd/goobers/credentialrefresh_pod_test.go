package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// TestPodDeclaredStageAsksForAndDeliversAGrantToACLIChild: dispatch-exec's
// stage-start resolve asks for a grant (with the attempt and timeout) only for
// a goobers-CLI child, hands the child the grant and its own daemon endpoint
// — never the pod token — and adds the grant to the scrubber.
func TestPodDeclaredStageAsksForAndDeliversAGrantToACLIChild(t *testing.T) {
	const grant = "goobers-grant.cGF5bG9hZC1wYXlsb2FkLXBheWxvYWQ.bWFjLW1hYy1tYWMtbWFjLW1hYw"
	for _, isCLI := range []bool{true, false} {
		var mu sync.Mutex
		var requests []dispatcher.CredentialResolveRequest
		plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request dispatcher.CredentialResolveRequest
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &request)
			mu.Lock()
			requests = append(requests, request)
			mu.Unlock()
			response := map[string]any{"credentials": []map[string]any{{"capability": "repo:push", "value": "minted-value", "expiresAt": time.Now().Add(time.Hour)}}}
			if request.Grant {
				response["grant"] = map[string]any{"token": grant, "expiresAt": time.Now().Add(2 * time.Hour)}
			}
			_ = json.NewEncoder(w).Encode(response)
		}))
		t.Cleanup(plane.Close)
		t.Setenv(dispatcher.EnvDaemonAPI, plane.URL)
		t.Setenv(dispatcher.EnvPodToken, "pod-token-never-for-the-child")
		t.Setenv(dispatcher.EnvRunID, "run-pod")
		t.Setenv(dispatcher.EnvStage, "push-branch")
		t.Setenv(dispatcher.EnvAttempt, "3")
		t.Setenv(dispatcher.EnvStageTimeout, "45m")
		t.Setenv(dispatcher.EnvStageCapabilities, `["repo:push"]`)
		t.Setenv(dispatcher.EnvStageIsCLI, map[bool]string{true: "true", false: "false"}[isCLI])

		resolved, err := resolveDeclaredStageCredentials(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		env := strings.Join(resolved.env(), "\n")
		if len(requests) != 1 || requests[0].Grant != isCLI {
			t.Fatalf("cli=%v: requests = %+v", isCLI, requests)
		}
		if strings.Contains(env, "pod-token-never-for-the-child") {
			t.Fatal("the pod token reached the child's environment")
		}
		if !isCLI {
			if strings.Contains(env, executor.CredentialGrantEnvVar) || resolved.grant != nil {
				t.Fatalf("a non-CLI child got a grant: %s", env)
			}
			continue
		}
		if requests[0].Attempt != 3 || requests[0].TimeoutSeconds != 45*60 {
			t.Fatalf("grant request = %+v, want attempt 3 and the 45m timeout", requests[0])
		}
		if !strings.Contains(env, executor.CredentialEndpointEnvVar+"="+plane.URL) || !strings.Contains(env, executor.CredentialGrantEnvVar+"="+grant) {
			t.Fatalf("child env lacks the endpoint or grant:\n%s", env)
		}
		scrubbed := podStageScrubber(resolved.withGrant(resolved.creds), resolved.scheme).Scrub([]byte("leak " + grant))
		if strings.Contains(string(scrubbed), grant) {
			t.Fatal("the grant is not scrubbed from the pod stage's output")
		}
	}
}

// TestPodRefreshRouteServesTheChildThroughTheRealClient: the child's
// CredentialRefreshClient presents the grant to the refresh path and reads
// the value back.
func TestPodRefreshRouteServesTheChildThroughTheRealClient(t *testing.T) {
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apicontract.CredentialRefreshPath || r.Header.Get("Authorization") != "Bearer goobers-grant.x.y" {
			http.Error(w, `{"error":{"code":"credential_refresh_requires_grant"}}`, http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"credentials": []map[string]any{{"capability": "repo:push", "value": "fresh"}}})
	}))
	t.Cleanup(plane.Close)
	minted, err := (&dispatcher.CredentialRefreshClient{BaseURL: plane.URL, Grant: "goobers-grant.x.y"}).Refresh(context.Background(), "repo:push")
	if err != nil || minted.Value != "fresh" {
		t.Fatalf("Refresh = %+v, %v", minted, err)
	}
	_, err = (&dispatcher.CredentialRefreshClient{BaseURL: plane.URL, Grant: "goobers-grant.wrong"}).Refresh(context.Background(), "repo:push")
	if err == nil {
		t.Fatal("a refused refresh returned a value")
	}
}

// TestLocalCIPollNoLongerSnapshotsItsToken is the #3489 shape on the local
// substrate: the daemon's in-process ci-poll used to resolve its PR token once
// for the whole poll. Now a token the forge stops honoring mid-poll is
// re-resolved through the daemon's own injector and the poll continues.
func TestLocalCIPollNoLongerSnapshotsItsToken(t *testing.T) {
	minted := &mintSequence{ttl: time.Hour}
	resolver, err := credentials.NewResolverWithExpiring(nil, nil, nil,
		map[string]credentials.ExpiringResolveFunc{"pr-ref": minted.next})
	if err != nil {
		t.Fatal(err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{{Capability: "provider:pr:write", Ref: "pr-ref"}}, &recordingRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	poller, err := localCIPollGitHubPoller(context.Background(), injector, "provider:pr:write", nil)
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := poller.(*providers.GitHubProvider)
	if !ok {
		t.Fatalf("poller = %T", poller)
	}
	forge := &fakeExpiringForge{valid: minted.value(1)}
	server := httptest.NewServer(http.HandlerFunc(forge.handler))
	t.Cleanup(server.Close)
	provider.BaseURL = server.URL
	if _, err := provider.AuthenticatedLogin(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	forge.expireAllBut(minted.value(2))
	if _, err := provider.AuthenticatedLogin(context.Background()); err != nil {
		t.Fatalf("poll after the token expired: %v; want it re-resolved through the injector", err)
	}
	if minted.calls != 2 {
		t.Fatalf("injector minted %d values, want the stage-start value and one re-resolve", minted.calls)
	}
}

type recordingRegistrar struct{ values []string }

func (r *recordingRegistrar) Register(secret []byte) { r.values = append(r.values, string(secret)) }
