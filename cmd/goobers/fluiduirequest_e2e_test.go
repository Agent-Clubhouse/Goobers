package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/fluidui"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/runtimewiring"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

type fluidUITestAuthenticator struct{}

func (fluidUITestAuthenticator) Authenticate(*http.Request) (*httpapi.Principal, error) {
	return &httpapi.Principal{Issuer: "oidc", Subject: "fluid-ui-user", Roles: []httpapi.Role{httpapi.RoleView}}, nil
}

// fluidUIDaemon runs the shipped fluid-ui-customize workflow behind the
// ordinary trigger plane and the Labs request routes. propose answers each
// propose-stage call (1-based) with the candidate file body to write.
type fluidUIDaemon struct {
	handler http.Handler
	settle  func()
	calls   func() int
}

func newFluidUIDaemon(t *testing.T, propose func(call int) string) *fluidUIDaemon {
	t.Helper()
	root := initDeterministicDemo(t)
	gaggleDir := filepath.Join(root, "config", "gaggles", "example")
	shipped := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers")
	for target, source := range map[string]string{
		filepath.Join(gaggleDir, "workflows", "fluid-ui-customize.yaml"):            filepath.Join(shipped, "workflows", "fluid-ui-customize.yaml"),
		filepath.Join(gaggleDir, "goobers", "fluid-ui-designer", "goober.yaml"):     filepath.Join(shipped, "goobers", "fluid-ui-designer", "goober.yaml"),
		filepath.Join(gaggleDir, "goobers", "fluid-ui-designer", "instructions.md"): filepath.Join(shipped, "goobers", "fluid-ui-designer", "instructions.md"),
	} {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, target, strings.Replace(string(data), "gaggle: goobers", "gaggle: example", 1))
	}

	var mu sync.Mutex
	calls := 0
	prevAdapter := runtimewiring.NewAgenticAdapter
	runtimewiring.NewAgenticAdapter = func(string, map[string]string) harness.Adapter {
		return &harnesstest.FakeAdapter{
			Transcript: []byte("fake fluid UI designer session\n"),
			Act: func(_ context.Context, req harness.RunRequest) error {
				mu.Lock()
				calls++
				n := calls
				mu.Unlock()
				artifact, _ := req.Envelope.Inputs["artifactFile"].(string)
				if artifact == "" {
					artifact = "fluid-ui-candidate.json"
				}
				if !filepath.IsAbs(artifact) {
					artifact = filepath.Join(req.Workspace, artifact)
				}
				if err := os.WriteFile(artifact, []byte(propose(n)), 0o644); err != nil {
					return err
				}
				return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "proposed"})
			},
		}
	}
	t.Cleanup(func() { runtimewiring.NewAgenticAdapter = prevAdapter })

	layout := instance.NewLayout(root)
	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(t.Context(), layout, &wg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := setup.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	sched := localscheduler.New(setup.Entries, setup.InstanceLog, setup.SchedulerOptions()...)
	dispatch := newDaemonTriggerService()
	dispatch.AttachScheduler(sched)
	dispatch.AttachDispatchContext(t.Context())
	service, _, cancels, err := newDaemonCoordinationServices(layout, dispatch, setup.RunnerRegistry, setup.InstanceLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.queue.Close(); _ = cancels.receipts.Close() })
	if err := setup.installOrdinaryStarts(layout, service); err != nil {
		t.Fatal(err)
	}
	opts := append([]httpapi.HandlerOption{httpapi.WithAuthenticator(fluidUITestAuthenticator{}), httpapi.WithTriggerService(service)}, labsFluidUIOptions(layout)...)
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return &fluidUIDaemon{
		handler: handler,
		settle: func() {
			t.Helper()
			if err := service.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			sched.Wait()
			wg.Wait()
		},
		calls: func() int { mu.Lock(); defer mu.Unlock(); return calls },
	}
}

func (d *fluidUIDaemon) call(t *testing.T, method, path, body string, want int, out any) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	d.handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, response.Code, want, response.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
}

// submitAndSettle submits prompt, lets the run finish, and returns the
// request as the API reports it afterwards.
func (d *fluidUIDaemon) submitAndSettle(t *testing.T, prompt string) fluidui.Request {
	t.Helper()
	var submitted fluidui.Request
	d.call(t, http.MethodPost, apicontract.LabsFluidUIRequestsPath, `{"prompt":`+jsonString(t, prompt)+`}`, http.StatusAccepted, &submitted)
	if submitted.Status != fluidui.StatusQueued || submitted.RunID == "" {
		t.Fatalf("submitted = %+v", submitted)
	}
	d.settle()
	var got fluidui.Request
	d.call(t, http.MethodGet, apicontract.LabsFluidUIRequestsPath+"/"+submitted.ID, "", http.StatusOK, &got)
	if got.RunID != submitted.RunID {
		t.Fatalf("request run changed: %+v", got)
	}
	return got
}

func jsonString(t *testing.T, value string) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFluidUIRequestReachesReadyToPreviewWithoutApplying(t *testing.T) {
	d := newFluidUIDaemon(t, func(int) string {
		return `{"version":"portal.customization/v1","density":"compact"}`
	})
	got := d.submitAndSettle(t, "make everything denser")
	if got.Status != fluidui.StatusReadyToPreview || got.Candidate == nil || got.Candidate.Density != fluidui.DensityCompact {
		t.Fatalf("request = %+v", got)
	}
	var state fluidui.State
	d.call(t, http.MethodGet, apicontract.LabsFluidUICustomizationPath, "", http.StatusOK, &state)
	if state.Customization != nil {
		t.Fatalf("the candidate was applied without the user: %+v", state)
	}
	var applied struct {
		Request       fluidui.Request `json:"request"`
		Customization fluidui.State   `json:"customization"`
	}
	d.call(t, http.MethodPost, apicontract.LabsFluidUIRequestsPath+"/"+got.ID+"/apply", "{}", http.StatusOK, &applied)
	if applied.Request.Status != fluidui.StatusApplied || applied.Customization.Customization == nil {
		t.Fatalf("apply = %+v", applied)
	}
}

func TestFluidUIRequestFailsAfterOneRetryOfInvalidOutput(t *testing.T) {
	d := newFluidUIDaemon(t, func(int) string { return "I rewrote the portal CSS for you." })
	got := d.submitAndSettle(t, "make it pop")
	if got.Status != fluidui.StatusFailed || got.Candidate != nil {
		t.Fatalf("request = %+v", got)
	}
	if calls := d.calls(); calls != 2 {
		t.Fatalf("propose ran %d times, want the first attempt plus exactly one retry", calls)
	}
	d.call(t, http.MethodPost, apicontract.LabsFluidUIRequestsPath+"/"+got.ID+"/apply", "{}", http.StatusConflict, nil)
}

func TestFluidUIRequestOutsideTheSurfaceIsLabeledUnsupported(t *testing.T) {
	d := newFluidUIDaemon(t, func(int) string { return `{"unsupported":true,"unmetNeed":"Cost Charts"}` })
	got := d.submitAndSettle(t, "show me a chart of my token spend")
	if got.Status != fluidui.StatusUnsupported || got.UnmetNeed != "cost-charts" || got.Candidate != nil {
		t.Fatalf("request = %+v", got)
	}
}
