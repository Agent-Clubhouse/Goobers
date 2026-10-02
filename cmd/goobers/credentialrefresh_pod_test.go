package main

import (
	"context"
	"encoding/json"
	"errors"
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

func TestMintedValueAndExpiry(t *testing.T) {
	expiry := time.Unix(1_700_000_000, 0)
	creds := []dispatcher.MintedCredential{
		{Capability: "repo:push", Value: "push-value"},
		{Capability: "provider:pr:write", Value: "pr-value", ExpiresAt: &expiry},
	}
	value, expiresAt, err := mintedValueAndExpiry(creds, "provider:pr:write")
	if err != nil || value != "pr-value" || !expiresAt.Equal(expiry) {
		t.Fatalf("mintedValueAndExpiry = %q, %v, %v; want the named capability's value and expiry", value, expiresAt, err)
	}
	value, expiresAt, err = mintedValueAndExpiry(creds, "repo:push")
	if err != nil || value != "push-value" || !expiresAt.IsZero() {
		t.Fatalf("mintedValueAndExpiry = %q, %v, %v; want the value with no stated expiry", value, expiresAt, err)
	}
	value, _, err = mintedValueAndExpiry(creds[:1], "provider:pr:write")
	if err == nil || err.Error() != `the credential plane returned no value for "provider:pr:write"` || value != "" {
		t.Fatalf("mintedValueAndExpiry = %q, %v; want the missing capability named", value, err)
	}
}

// TestPodCIPollTokenSourceWithoutAUsableValue: no value for the capability
// (or a blank one) is no source, so ci-poll keeps its non-refreshing path.
func TestPodCIPollTokenSourceWithoutAUsableValue(t *testing.T) {
	for name, creds := range map[string][]dispatcher.MintedCredential{
		"none":             nil,
		"other capability": {{Capability: "repo:push", Value: "push-value"}},
		"blank value":      {{Capability: "provider:pr:write", Value: "   "}},
	} {
		if source := podCIPollTokenSource(creds, "provider:pr:write", nil); source != nil {
			t.Errorf("%s: source = %#v, want nil", name, source)
		}
	}
}

// TestPodCIPollTokenSourceRefreshThroughThePlane: a value with a stated
// expiry keeps it; after a 401 the source re-resolves through the credential
// plane for its own run and stage, takes the fresh value and expiry, and
// reports a plane it cannot reach, a plane failure, or a plane answer without
// the capability as a rejected credential that was not refreshed.
func TestPodCIPollTokenSourceRefreshThroughThePlane(t *testing.T) {
	const capabilityName = "provider:pr:write"
	stageExpiry := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	freshExpiry := stageExpiry.Add(time.Hour)
	cases := map[string]struct {
		handler   http.HandlerFunc
		noPlane   bool
		wantValue string
		wantErr   string
	}{
		"fresh value": {handler: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"credentials": []map[string]any{
				{"capability": "repo:push", "value": "unrelated"},
				{"capability": capabilityName, "value": "fresh-value", "expiresAt": freshExpiry},
			}})
		}, wantValue: "fresh-value"},
		"plane unreachable": {noPlane: true, wantErr: dispatcher.EnvDaemonAPI + " is unset"},
		"plane fails": {handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":{"code":"internal"}}`, http.StatusInternalServerError)
		}, wantErr: "500"},
		"plane omits the capability": {handler: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"credentials": []map[string]any{{"capability": "repo:push", "value": "unrelated"}}})
		}, wantErr: `the credential plane returned no value for "provider:pr:write"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var requests []dispatcher.CredentialResolveRequest
			t.Setenv(dispatcher.EnvRunID, "run-pod")
			t.Setenv(dispatcher.EnvStage, "ci-poll")
			t.Setenv(dispatcher.EnvPodToken, "pod-token")
			t.Setenv(dispatcher.EnvDaemonAPI, "")
			if !tc.noPlane {
				plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request dispatcher.CredentialResolveRequest
					_ = json.NewDecoder(r.Body).Decode(&request)
					requests = append(requests, request)
					tc.handler(w, r)
				}))
				t.Cleanup(plane.Close)
				t.Setenv(dispatcher.EnvDaemonAPI, plane.URL)
			}
			registrar := &recordingRegistrar{}
			source := podCIPollTokenSource([]dispatcher.MintedCredential{{Capability: capabilityName, Value: "stage-value", ExpiresAt: &stageExpiry}}, capabilityName, registrar)
			token, ok := source.(*credentials.RefreshingToken)
			if !ok {
				t.Fatalf("source = %T, want a refreshing token", source)
			}
			if !token.Expiry().Equal(stageExpiry) {
				t.Fatalf("expiry = %v, want the stated %v", token.Expiry(), stageExpiry)
			}
			if value, err := token.Token(context.Background()); err != nil || value != "stage-value" || len(requests) != 0 {
				t.Fatalf("Token = %q, %v after %d plane calls; want the stage-start value without a call", value, err, len(requests))
			}

			token.Invalidate()
			value, err := token.Token(context.Background())
			if tc.wantErr != "" {
				if !errors.Is(err, credentials.ErrRejectedCredentialNotRefreshed) || !strings.Contains(err.Error(), tc.wantErr) || value != "" {
					t.Fatalf("Token after 401 = %q, %v; want a not-refreshed error naming %q", value, err, tc.wantErr)
				}
				if len(registrar.values) != 0 {
					t.Fatalf("registered %q, want nothing from a failed refresh", registrar.values)
				}
				return
			}
			if err != nil || value != tc.wantValue || !token.Expiry().Equal(freshExpiry) {
				t.Fatalf("Token after 401 = %q (expires %v), %v; want %q expiring %v", value, token.Expiry(), err, tc.wantValue, freshExpiry)
			}
			if len(requests) != 1 || requests[0].RunID != "run-pod" || requests[0].Stage != "ci-poll" ||
				strings.Join(requests[0].Capabilities, ",") != capabilityName || requests[0].Grant {
				t.Fatalf("plane requests = %+v, want one re-resolve of %s for run-pod/ci-poll without a grant", requests, capabilityName)
			}
			if strings.Join(registrar.values, ",") != "fresh-value" {
				t.Fatalf("registered %q, want the fresh value", registrar.values)
			}
		})
	}
}

// TestLocalCIPollTokenSourceResolveFailures: ci-poll's local token source and
// poller fail with the resolve error rather than polling without a token.
func TestLocalCIPollTokenSourceResolveFailures(t *testing.T) {
	const capabilityName = "provider:pr:write"
	resolveErr := errors.New("app key unreadable")
	cases := map[string]struct {
		resolve credentials.ExpiringResolveFunc
		grants  []credentials.Grant
		check   func(error) bool
	}{
		"resolver fails": {
			resolve: func(context.Context) (string, time.Time, error) { return "", time.Time{}, resolveErr },
			grants:  []credentials.Grant{{Capability: capabilityName, Ref: "pr-ref"}},
			check: func(err error) bool {
				return errors.Is(err, resolveErr) && strings.HasPrefix(err.Error(), "resolve ci-poll credentials: ")
			},
		},
		"capability not granted": {
			resolve: func(context.Context) (string, time.Time, error) { return "unused", time.Time{}, nil },
			check: func(err error) bool {
				return errors.Is(err, credentials.ErrNoCredentialForCapability) && strings.HasPrefix(err.Error(), "resolve ci-poll credential: ")
			},
		},
		"blank value": {
			resolve: func(context.Context) (string, time.Time, error) { return " ", time.Now().Add(time.Hour), nil },
			grants:  []credentials.Grant{{Capability: capabilityName, Ref: "pr-ref"}},
			check:   func(err error) bool { return errors.Is(err, credentials.ErrTokenRefEmpty) },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resolver, err := credentials.NewResolverWithExpiring(nil, nil, nil, map[string]credentials.ExpiringResolveFunc{"pr-ref": tc.resolve})
			if err != nil {
				t.Fatal(err)
			}
			injector, err := credentials.NewInjector(resolver, tc.grants, &recordingRegistrar{})
			if err != nil {
				t.Fatal(err)
			}
			token, source, err := ciPollTokenSource(context.Background(), injector, capabilityName, nil)
			if err == nil || !tc.check(err) || token != "" || source != nil {
				t.Fatalf("ciPollTokenSource = %q, %v, %v; want the classified resolve error", token, source, err)
			}
			poller, err := localCIPollGitHubPoller(context.Background(), injector, capabilityName, nil)
			if err == nil || !tc.check(err) || poller != nil {
				t.Fatalf("localCIPollGitHubPoller = %v, %v; want the same error and no poller", poller, err)
			}
		})
	}
}

type recordingRegistrar struct{ values []string }

func (r *recordingRegistrar) Register(secret []byte) { r.values = append(r.values, string(secret)) }
