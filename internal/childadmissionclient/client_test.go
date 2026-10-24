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

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/enginechildplane"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
)

// unusedReader satisfies the handler's read service; the plane never reads.
type unusedReader struct{ readservice.Reader }

// recordingAdmissions is the control-side custody owner behind the route.
type recordingAdmissions struct {
	admitted  []engine.ChildAdmissionInput
	loaded    []engine.ChildRunBinding
	cancelled int
	manifest  []byte
	fail      error
}

func (r *recordingAdmissions) Admit(_ context.Context, in engine.ChildAdmissionInput) (engine.ChildRunBinding, error) {
	r.admitted = append(r.admitted, in)
	return engine.ChildRunBinding{Request: in.Request, ManifestDigest: journal.Digest(r.manifest), ParentWorkflowID: in.ParentWorkflowID}, r.fail
}
func (r *recordingAdmissions) Load(_ context.Context, b engine.ChildRunBinding) ([]byte, error) {
	r.loaded = append(r.loaded, b)
	return r.manifest, r.fail
}
func (r *recordingAdmissions) Cancel(context.Context, engine.ChildRunBinding) error {
	r.cancelled++
	return r.fail
}

func planeServer(t *testing.T, backend engine.ChildAdmissions) (*podauth.SignedKey, string) {
	t.Helper()
	signer, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(signer, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	plane, err := enginechildplane.New(backend)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(unusedReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithEngineChildAdmissions(plane))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return signer, server.URL
}

func TestClientRoundTripsThroughTheAuthenticatedPlane(t *testing.T) {
	backend := &recordingAdmissions{manifest: []byte(`{"version":1}`)}
	signer, url := planeServer(t, backend)
	client := &Client{BaseURL: url, TokenSource: func() (string, error) { return signer.MintWorkerChildAdmission("engine-worker", time.Minute) }}
	in := engine.ChildAdmissionInput{ParentWorkflow: "parent", ParentWorkflowID: "parent-wf", ParentTemporalRunID: "trun", Stage: "parent", OriginSeq: 7}
	in.Request.ChildRunID = "child-1"
	binding, err := client.Admit(t.Context(), in)
	if err != nil || binding.Request.ChildRunID != "child-1" || len(backend.admitted) != 1 || backend.admitted[0] != in {
		t.Fatalf("admit = %+v %v (backend saw %+v)", binding, err, backend.admitted)
	}
	manifest, err := client.Load(t.Context(), binding)
	if err != nil || string(manifest) != `{"version":1}` || len(backend.loaded) != 1 || backend.loaded[0].ManifestDigest != binding.ManifestDigest {
		t.Fatalf("load = %q %v", manifest, err)
	}
	if err := client.Cancel(t.Context(), binding); err != nil || backend.cancelled != 1 {
		t.Fatalf("cancel = %v (%d)", err, backend.cancelled)
	}
	backend.fail = errors.New("custody unavailable")
	if _, err := client.Admit(t.Context(), in); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("backend failure surfaced as %v", err)
	}
}

func TestPlaneRefusesEveryOtherCredential(t *testing.T) {
	backend := &recordingAdmissions{}
	signer, url := planeServer(t, backend)
	for name, mint := range map[string]func() (string, error){
		"pod":              func() (string, error) { return signer.Mint("run-1", time.Minute) },
		"worker surrender": func() (string, error) { return signer.MintWorkerSurrender("engine-worker", time.Minute) },
		"worker blob":      func() (string, error) { return signer.MintWorkerBlob("engine-worker", time.Minute) },
		"none":             func() (string, error) { return "unauthenticated", nil },
	} {
		t.Run(name, func(t *testing.T) {
			client := &Client{BaseURL: url, TokenSource: mint}
			if _, err := client.Admit(t.Context(), engine.ChildAdmissionInput{}); err == nil {
				t.Fatal("plane admitted a foreign credential")
			}
		})
	}
	if len(backend.admitted) != 0 {
		t.Fatalf("backend reached by foreign credentials: %d", len(backend.admitted))
	}
}

func TestUnservedCustodyOperationsFailClosed(t *testing.T) {
	c := &Client{}
	if err := c.Park(t.Context(), engine.ChildParkInput{}); !errors.Is(err, ErrCustodyUnmapped) {
		t.Fatal(err)
	}
	if _, err := c.Finalize(t.Context(), engine.ChildFinalizeInput{}); !errors.Is(err, ErrCustodyUnmapped) {
		t.Fatal(err)
	}
	if _, err := c.Complete(t.Context(), engine.ChildCompletionInput{}); !errors.Is(err, ErrCustodyUnmapped) {
		t.Fatal(err)
	}
	if _, err := c.Admit(t.Context(), engine.ChildAdmissionInput{}); err == nil {
		t.Fatal("unconfigured client called out")
	}
}

func TestClientCarriesCustodyErrorClassesAcrossThePlane(t *testing.T) {
	backend := &recordingAdmissions{}
	signer, url := planeServer(t, backend)
	client := &Client{BaseURL: url, TokenSource: func() (string, error) { return signer.MintWorkerChildAdmission("engine-worker", time.Minute) }}
	for _, tc := range []struct {
		name               string
		fail               error
		unavailable, final bool
	}{
		{name: "failover", fail: fmt.Errorf("lease lost: %w", engine.ErrChildCustodyUnavailable), unavailable: true},
		{name: "refusal", fail: fmt.Errorf("parent closed: %w", engine.ErrChildCustodyRefused), final: true},
		{name: "unclassified", fail: errors.New("custody bug")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend.fail = tc.fail
			_, err := client.Admit(t.Context(), engine.ChildAdmissionInput{})
			if err == nil || errors.Is(err, engine.ErrChildCustodyUnavailable) != tc.unavailable || errors.Is(err, engine.ErrChildCustodyRefused) != tc.final {
				t.Fatalf("admit error = %v, want unavailable=%v refused=%v", err, tc.unavailable, tc.final)
			}
		})
	}
}

func TestClientTreatsAnUnreachablePlaneAsFailover(t *testing.T) {
	client := &Client{BaseURL: "http://127.0.0.1:1", TokenSource: func() (string, error) { return "token", nil }}
	if _, err := client.Admit(t.Context(), engine.ChildAdmissionInput{}); !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("unreachable plane error = %v, want unavailable", err)
	}
}

func TestStatusClassRefusesOnlyTheExplicitConflict(t *testing.T) {
	for status, want := range map[int]error{
		http.StatusServiceUnavailable: engine.ErrChildCustodyUnavailable, http.StatusTooManyRequests: engine.ErrChildCustodyUnavailable,
		http.StatusBadGateway: engine.ErrChildCustodyUnavailable, http.StatusGatewayTimeout: engine.ErrChildCustodyUnavailable,
		// A route missing mid-rollout is retryable; only the plane's explicit
		// refusal is permanent.
		http.StatusNotFound: engine.ErrChildCustodyUnavailable, http.StatusMethodNotAllowed: engine.ErrChildCustodyUnavailable,
		http.StatusConflict:   engine.ErrChildCustodyRefused,
		http.StatusBadRequest: nil, http.StatusUnprocessableEntity: nil,
		http.StatusUnauthorized: nil, http.StatusForbidden: nil, http.StatusInternalServerError: nil,
	} {
		if got := statusClass(status); !errors.Is(got, want) || (want == nil) != (got == nil) {
			t.Errorf("statusClass(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestForWorkerMintsOnlyTheChildAdmissionBearer(t *testing.T) {
	signer, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if got := ForWorker("http://control", "worker-0", nil); got != nil {
		t.Fatalf("no signing key produced %#v, want an untyped nil", got)
	}
	if got := ForWorker("", "worker-0", signer); got != nil {
		t.Fatalf("no plane endpoint produced %#v, want an untyped nil", got)
	}
	client, ok := ForWorker("http://control", "worker-0", signer).(*Client)
	if !ok || client.BaseURL != "http://control" {
		t.Fatalf("client = %#v", client)
	}
	token, err := client.TokenSource()
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(signer, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	principal, err := auth.Authenticate(request)
	if err != nil || principal.Issuer != httpapi.WorkerChildAdmissionPrincipalIssuer {
		t.Fatalf("minted bearer authenticated as %+v (%v), want the child-admission issuer", principal, err)
	}
}

func TestClientRetriesAnAPIThatDoesNotServeThePlaneYet(t *testing.T) {
	// An older API replica mid-rollout has no such route.
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	client := &Client{BaseURL: server.URL, TokenSource: func() (string, error) { return "token", nil }}
	if _, err := client.Admit(t.Context(), engine.ChildAdmissionInput{}); !errors.Is(err, engine.ErrChildCustodyUnavailable) || errors.Is(err, engine.ErrChildCustodyRefused) {
		t.Fatalf("missing route error = %v, want retryable", err)
	}
}

func TestWorkerBearerOutlivesOneChildActivityAttempt(t *testing.T) {
	// The bearer is minted once per call and must stay valid for the whole
	// activity attempt that carries it.
	if workerTokenTTL < engine.ChildActivityStartToClose {
		t.Fatalf("worker bearer TTL %s is shorter than the child activity StartToClose %s", workerTokenTTL, engine.ChildActivityStartToClose)
	}
	if workerTokenTTL > podauth.MaxWorkerTokenTTL {
		t.Fatalf("worker bearer TTL %s exceeds the verifier maximum %s", workerTokenTTL, podauth.MaxWorkerTokenTTL)
	}
}
