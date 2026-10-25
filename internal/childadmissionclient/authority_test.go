package childadmissionclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/enginechildplane"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
)

// recordingAuthority is the control-side grant owner behind the route.
type recordingAuthority struct {
	acquired []engine.ChildAuthorityRequest
	released []engine.ChildAuthorityRelease
	observed []engine.ChildHandoffObservation
	handoff  *journal.ChildHandoffRequest
	grant    engine.ChildAuthorityGrant
	fail     error
}

func (r *recordingAuthority) AcquireChildAuthority(_ context.Context, request engine.ChildAuthorityRequest) (engine.ChildAuthorityGrant, error) {
	r.acquired = append(r.acquired, request)
	return r.grant, r.fail
}

func (r *recordingAuthority) ReleaseChildAuthority(_ context.Context, release engine.ChildAuthorityRelease) error {
	r.released = append(r.released, release)
	return r.fail
}

const authorityRun = "0123456789abcdef0123456789abcdef"

func authorityRequest() engine.ChildAuthorityRequest {
	return engine.ChildAuthorityRequest{RunID: authorityRun, Origin: apiv1.ChildWorkflowOrigin{StageOccurrence: "occ-1", AttemptID: "att-1"},
		Endpoint: "https://control.example"}
}

func authorityServer(t *testing.T, backend engine.ChildWorkflowAuthority) (*podauth.SignedKey, string) {
	t.Helper()
	signer, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	// Child-workflow grants authenticate on this server, so the refusal
	// below is the authorizer's, not a failed signature.
	auth, err := podauth.NewAuthenticator(signer, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	auth = auth.WithChildWorkflowGrants(signer)
	options := []httpapi.HandlerOption{httpapi.WithAuthenticator(auth)}
	if backend != nil {
		authority, err := enginechildplane.NewAuthority(backend)
		if err != nil {
			t.Fatal(err)
		}
		options = append(options, httpapi.WithEngineChildAuthority(authority))
	}
	handler, err := httpapi.NewHandler(unusedReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), options...)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return signer, server.URL
}

// registry records registered secrets.
type registry struct{ secrets []string }

func (r *registry) RegisterUntil(secret []byte, _ time.Time) {
	r.secrets = append(r.secrets, string(secret))
}

func workerClient(signer *podauth.SignedKey, url string) *Client {
	return &Client{BaseURL: url, Secrets: &registry{}, TokenSource: func() (string, error) { return signer.MintWorkerChildAdmission("engine-worker", time.Minute) }}
}

func TestAuthorityRoundTripsThroughTheAuthenticatedPlane(t *testing.T) {
	backend := &recordingAuthority{grant: engine.ChildAuthorityGrant{Endpoint: "https://control.example", BearerToken: "goobers-child.signed",
		GrantID: "grant-1", ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Second).UTC()}}
	signer, url := authorityServer(t, backend)
	client := workerClient(signer, url)
	grant, err := client.AcquireChildAuthority(t.Context(), authorityRequest())
	if err != nil || grant != backend.grant || len(backend.acquired) != 1 || backend.acquired[0] != authorityRequest() {
		t.Fatalf("acquire = %v, %v (backend saw %+v)", grant, err, backend.acquired)
	}
	release := engine.ChildAuthorityRelease{RunID: authorityRun, Origin: authorityRequest().Origin, GrantID: "grant-1"}
	if err := client.ReleaseChildAuthority(t.Context(), release); err != nil || len(backend.released) != 1 || backend.released[0] != release {
		t.Fatalf("release = %v (backend saw %+v)", err, backend.released)
	}
}

func TestAuthorityResponseIsNeverCached(t *testing.T) {
	backend := &recordingAuthority{grant: engine.ChildAuthorityGrant{Endpoint: "https://control.example", BearerToken: "goobers-child.signed", GrantID: "grant-1"}}
	signer, url := authorityServer(t, backend)
	token, err := signer.MintWorkerChildAdmission("engine-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, url+apicontract.EngineChildAuthorityPath, strings.NewReader(
		`{"runId":"`+authorityRun+`","origin":{"stageOccurrence":"occ-1","attemptId":"att-1"},"endpoint":"https://control.example"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("status %d, Cache-Control %q; a bearer response must not be stored", response.StatusCode, response.Header.Get("Cache-Control"))
	}
}

// Only the worker child-admission credential reaches the authority route: not
// a stage pod, not another worker credential, and not the child-workflow
// grant the route itself issues.
func TestAuthorityRefusesEveryOtherCredential(t *testing.T) {
	backend := &recordingAuthority{}
	signer, url := authorityServer(t, backend)
	for name, mint := range map[string]func() (string, error){
		"pod":              func() (string, error) { return signer.Mint("run-1", time.Minute) },
		"worker surrender": func() (string, error) { return signer.MintWorkerSurrender("engine-worker", time.Minute) },
		"worker blob":      func() (string, error) { return signer.MintWorkerBlob("engine-worker", time.Minute) },
		"child-workflow grant": func() (string, error) {
			token, _, err := signer.MintChildWorkflowGrant(podauth.ChildWorkflowGrant{Gaggle: "example", RunID: authorityRun,
				StageOccurrence: "occ-1", AttemptID: "att-1", ConfigDigest: "sha256:" + strings.Repeat("a", 64), PolicyDigest: "sha256:" + strings.Repeat("b", 64)}, time.Minute)
			return token, err
		},
		"none": func() (string, error) { return "unauthenticated", nil },
	} {
		t.Run(name, func(t *testing.T) {
			client := &Client{BaseURL: url, Secrets: &registry{}, TokenSource: mint}
			_, err := client.AcquireChildAuthority(t.Context(), authorityRequest())
			if err == nil {
				t.Fatal("authority route admitted a foreign credential")
			}
			if _, err := client.ObserveChildHandoff(t.Context(), engine.ChildHandoffObservation{ChildAuthorityRelease: engine.ChildAuthorityRelease{RunID: authorityRun, Origin: authorityRequest().Origin, GrantID: "grant-1"}}); err == nil {
				t.Fatal("handoff route admitted a foreign credential")
			}
			// The grant authenticates here; the authorizer must be what refuses it.
			if name == "child-workflow grant" && !strings.Contains(err.Error(), "status 403") {
				t.Fatalf("child-workflow grant refused by %v, want the authorizer's 403", err)
			}
			release := engine.ChildAuthorityRelease{RunID: authorityRun, Origin: authorityRequest().Origin, GrantID: "grant-1"}
			if err := client.ReleaseChildAuthority(t.Context(), release); err == nil {
				t.Fatal("release route admitted a foreign credential")
			}
		})
	}
	if len(backend.acquired) != 0 || len(backend.released) != 0 {
		t.Fatalf("backend reached by foreign credentials: %d acquires, %d releases", len(backend.acquired), len(backend.released))
	}
}

func TestAuthorityCarriesCustodyErrorClassesAcrossThePlane(t *testing.T) {
	backend := &recordingAuthority{}
	signer, url := authorityServer(t, backend)
	client := workerClient(signer, url)
	for _, tc := range []struct {
		name               string
		fail               error
		unavailable, final bool
	}{
		{name: "failover", fail: fmt.Errorf("lease lost: %w", engine.ErrChildCustodyUnavailable), unavailable: true},
		{name: "refusal", fail: fmt.Errorf("attempt superseded: %w", engine.ErrChildCustodyRefused), final: true},
		{name: "unclassified", fail: errors.New("authority bug")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend.fail = tc.fail
			_, err := client.AcquireChildAuthority(t.Context(), authorityRequest())
			if err == nil || errors.Is(err, engine.ErrChildCustodyUnavailable) != tc.unavailable || errors.Is(err, engine.ErrChildCustodyRefused) != tc.final {
				t.Fatalf("acquire error = %v, want unavailable=%v refused=%v", err, tc.unavailable, tc.final)
			}
		})
	}
}

func TestAuthorityOnAServerWithoutItIsUnavailable(t *testing.T) {
	signer, url := authorityServer(t, nil)
	if _, err := workerClient(signer, url).AcquireChildAuthority(t.Context(), authorityRequest()); !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("acquire on a server without authority = %v, want unavailable", err)
	}
}

func TestAuthorityRefusesMalformedRequestsAndGrants(t *testing.T) {
	backend := &recordingAuthority{grant: engine.ChildAuthorityGrant{Endpoint: "https://control.example", BearerToken: "not-a-child-grant", GrantID: "grant-1"}}
	signer, url := authorityServer(t, backend)
	client := workerClient(signer, url)
	bad := authorityRequest()
	bad.Endpoint = "https://control.example/with/a/path"
	if _, err := client.AcquireChildAuthority(t.Context(), bad); !errors.Is(err, engine.ErrChildCustodyRefused) || len(backend.acquired) != 0 {
		t.Fatalf("malformed request = %v (sent %d)", err, len(backend.acquired))
	}
	if _, err := client.AcquireChildAuthority(t.Context(), authorityRequest()); err == nil {
		t.Fatal("client accepted a grant that is not a child-workflow bearer")
	}
	if got := fmt.Sprintf("%v %+v %#v", backend.grant, backend.grant, backend.grant); strings.Contains(got, backend.grant.BearerToken) {
		t.Fatalf("formatted grant leaks its bearer: %s", got)
	}
}

func TestAuthorityForWorkerFillsThePodEndpointAndRegistersTheBearer(t *testing.T) {
	backend := &recordingAuthority{grant: engine.ChildAuthorityGrant{Endpoint: "https://control.example", BearerToken: "goobers-child.signed", GrantID: "grant-1"}}
	signer, url := authorityServer(t, backend)
	secrets := &registry{}
	client := AuthorityForWorker(url, "https://control.example", "engine-worker", signer, secrets)
	request := authorityRequest()
	request.Endpoint = ""
	if _, err := client.AcquireChildAuthority(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if len(backend.acquired) != 1 || backend.acquired[0].Endpoint != "https://control.example" {
		t.Fatalf("control saw %+v, want the pod endpoint filled in", backend.acquired)
	}
	if len(secrets.secrets) != 1 || secrets.secrets[0] != "goobers-child.signed" {
		t.Fatalf("registered %v, want the acquired bearer", secrets.secrets)
	}
	for name, client := range map[string]engine.ChildWorkflowAuthority{
		"no key":      AuthorityForWorker(url, "https://control.example", "w", nil, secrets),
		"no endpoint": AuthorityForWorker(url, "", "w", signer, secrets),
		"no registry": AuthorityForWorker(url, "https://control.example", "w", signer, nil),
		"no control":  AuthorityForWorker("", "https://control.example", "w", signer, secrets),
	} {
		if client != nil {
			t.Fatalf("%s: AuthorityForWorker returned a client", name)
		}
	}
}

func (r *recordingAuthority) ObserveChildHandoff(_ context.Context, request engine.ChildHandoffObservation) (*journal.ChildHandoffRequest, error) {
	r.observed = append(r.observed, request)
	return r.handoff, r.fail
}

func TestHandoffRoundTripsOnlyThroughWorkerPlane(t *testing.T) {
	backend := &recordingAuthority{handoff: &journal.ChildHandoffRequest{ParentRunID: authorityRun, Origin: authorityRequest().Origin}}
	signer, url := authorityServer(t, backend)
	client := workerClient(signer, url)
	request := engine.ChildHandoffObservation{ChildAuthorityRelease: engine.ChildAuthorityRelease{RunID: authorityRun, Origin: authorityRequest().Origin, GrantID: "bound-grant"}, Finish: true}
	got, err := client.ObserveChildHandoff(t.Context(), request)
	if err != nil || got == nil || *got != *backend.handoff || len(backend.observed) != 1 || backend.observed[0] != request {
		t.Fatalf("handoff=%+v error=%v requests=%+v", got, err, backend.observed)
	}
	backend.handoff = nil
	if got, err := client.ObserveChildHandoff(t.Context(), request); err != nil || got != nil {
		t.Fatalf("no acceptance=%+v error=%v", got, err)
	}
}

func TestAuthorityRecoversStoppedHandoffWithoutABearer(t *testing.T) {
	h := journal.ChildHandoffRequest{Gaggle: "example", ParentRunID: authorityRun, Origin: authorityRequest().Origin, Action: "wait", ChildRunID: "fedcba9876543210fedcba9876543210", AcceptanceID: "trigger-fedcba9876543210fedcba9876543210", InvocationKey: "first", SourceDigest: journal.Digest([]byte("source"))}
	h.RequestID = engine.ChildHandoffDigest(h)
	backend := &recordingAuthority{grant: engine.ChildAuthorityGrant{GrantID: "bound", StoppedHandoff: &h}}
	signer, url := authorityServer(t, backend)
	client := workerClient(signer, url)
	got, err := client.AcquireChildAuthority(t.Context(), authorityRequest())
	if err != nil || got.StoppedHandoff == nil || *got.StoppedHandoff != h || got.BearerToken != "" || len(client.Secrets.(*registry).secrets) != 0 {
		t.Fatalf("recovery=%+v %v", got.StoppedHandoff, err)
	}
	h.Origin.AttemptID = "forged-attempt"
	if _, err := client.AcquireChildAuthority(t.Context(), authorityRequest()); !errors.Is(err, engine.ErrChildCustodyRefused) {
		t.Fatalf("forged recovery accepted: %v", err)
	}
}
