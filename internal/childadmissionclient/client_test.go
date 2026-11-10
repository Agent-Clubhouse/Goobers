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
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/enginechildplane"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/triggerqueue"
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

// recordingProtocol is the control-side child protocol owner behind the routes.
type recordingProtocol struct {
	parked    []engine.ChildParkInput
	finalized []engine.ChildFinalizeInput
	completed []engine.ChildCompletionInput
	fail      error
}

func (r *recordingProtocol) Park(_ context.Context, in engine.ChildParkInput) error {
	r.parked = append(r.parked, in)
	return r.fail
}
func (r *recordingProtocol) Finalize(_ context.Context, in engine.ChildFinalizeInput) (childresult.TerminalReturn, error) {
	r.finalized = append(r.finalized, in)
	return childresult.TerminalReturn{Version: childresult.TerminalReturnVersion, RunID: in.Binding.Child.RunID, Input: childresult.TerminalResultInput{State: in.State, Summary: "sealed"}}, r.fail
}
func (r *recordingProtocol) Complete(_ context.Context, in engine.ChildCompletionInput) (apiv1.ContextPointer, error) {
	r.completed = append(r.completed, in)
	return apiv1.ContextPointer{Name: "child-result", Artifact: &apiv1.ArtifactPointer{Path: "child-results/r.json", Digest: in.Result.ResultRef, Size: 10}}, r.fail
}

func protocolServer(t *testing.T, protocol enginechildplane.ChildProtocol) (*podauth.SignedKey, string) {
	t.Helper()
	signer, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(signer, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	plane, err := enginechildplane.NewProtocol(protocol)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(unusedReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithEngineChildProtocol(plane))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return signer, server.URL
}

// Park, Finalize and Complete reach control's child protocol through the
// authenticated plane and carry its custody error classes back.
func TestChildProtocolRoundTripsThroughThePlane(t *testing.T) {
	backend := &recordingProtocol{}
	signer, url := protocolServer(t, backend)
	client := &Client{BaseURL: url, TokenSource: func() (string, error) { return signer.MintWorkerChildAdmission("engine-worker", time.Minute) }}
	var binding engine.ChildRunBinding
	binding.Request.RequestID = journal.Digest([]byte("request"))
	binding.Child.RunID = "child-1"
	wait := livejournal.AppendReceipt{Key: "wait", Seq: 7, Type: journal.EventRunnerAnnotation}
	if err := client.Park(t.Context(), engine.ChildParkInput{Binding: binding, Wait: wait}); err != nil || len(backend.parked) != 1 || backend.parked[0].Wait != wait {
		t.Fatalf("park = %v (backend saw %+v)", err, backend.parked)
	}
	sealed, err := client.Finalize(t.Context(), engine.ChildFinalizeInput{Binding: binding, State: triggerqueue.ChildCompleted})
	if err != nil || sealed.RunID != "child-1" || sealed.Input.Summary != "sealed" || len(backend.finalized) != 1 {
		t.Fatalf("finalize = %+v, %v", sealed, err)
	}
	sealed.ResultRef = journal.Digest([]byte("receipt"))
	pointer, err := client.Complete(t.Context(), engine.ChildCompletionInput{Binding: binding, Wait: wait, Result: sealed})
	if err != nil || pointer.Artifact == nil || pointer.Artifact.Digest != sealed.ResultRef || len(backend.completed) != 1 || backend.completed[0].Result.ResultRef != sealed.ResultRef {
		t.Fatalf("complete = %+v, %v", pointer, err)
	}
	for _, tc := range []struct {
		fail  error
		class error
	}{
		{fmt.Errorf("pool full: %w", engine.ErrChildCustodyUnavailable), engine.ErrChildCustodyUnavailable},
		{fmt.Errorf("forged: %w", engine.ErrChildCustodyRefused), engine.ErrChildCustodyRefused},
	} {
		backend.fail = tc.fail
		if err := client.Park(t.Context(), engine.ChildParkInput{Binding: binding, Wait: wait}); !errors.Is(err, tc.class) {
			t.Fatalf("park failure %v surfaced as %v", tc.fail, err)
		}
		if _, err := client.Finalize(t.Context(), engine.ChildFinalizeInput{Binding: binding}); !errors.Is(err, tc.class) {
			t.Fatalf("finalize failure %v surfaced as %v", tc.fail, err)
		}
		if _, err := client.Complete(t.Context(), engine.ChildCompletionInput{Binding: binding}); !errors.Is(err, tc.class) {
			t.Fatalf("complete failure %v surfaced as %v", tc.fail, err)
		}
	}
}

// A control plane that does not serve the protocol (an older one, or one
// whose capacity plane is not composed) answers retryably, never as success.
func TestChildProtocolUnservedIsRetryable(t *testing.T) {
	signer, url := planeServer(t, &recordingAdmissions{})
	client := &Client{BaseURL: url, TokenSource: func() (string, error) { return signer.MintWorkerChildAdmission("engine-worker", time.Minute) }}
	if err := client.Park(t.Context(), engine.ChildParkInput{}); !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("park on a server without the protocol = %v, want unavailable", err)
	}
	if _, err := client.Finalize(t.Context(), engine.ChildFinalizeInput{}); !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("finalize on a server without the protocol = %v, want unavailable", err)
	}
	if _, err := client.Complete(t.Context(), engine.ChildCompletionInput{}); !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("complete on a server without the protocol = %v, want unavailable", err)
	}
	if _, err := (&Client{}).Admit(t.Context(), engine.ChildAdmissionInput{}); err == nil {
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
