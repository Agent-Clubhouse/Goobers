package childadmissionclient

import (
	"context"
	"errors"
	"io"
	"log"
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
