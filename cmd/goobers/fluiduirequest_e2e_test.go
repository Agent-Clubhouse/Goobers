package main

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/fluidui"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/runtimewiring"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

type fluidUITestAuthenticator struct{}

// fluidUIPrincipalHeader names another view-role user for one request.
const fluidUIPrincipalHeader = "X-Test-Principal"

func (fluidUITestAuthenticator) Authenticate(request *http.Request) (*httpapi.Principal, error) {
	subject := cmp.Or(request.Header.Get(fluidUIPrincipalHeader), "fluid-ui-user")
	return &httpapi.Principal{Issuer: "oidc", Subject: subject, Roles: []httpapi.Role{httpapi.RoleView}}, nil
}

// fluidUIEchoingAgent stands in for an agent that reads the private request
// text and then repeats it everywhere it can: its transcript, its stderr and
// its completion summary.
type fluidUIEchoingAgent struct {
	*harnesstest.FakeAdapter
	received func(string)
}

func (a fluidUIEchoingAgent) Run(ctx context.Context, req harness.RunRequest) (harness.Outcome, error) {
	prompt, err := os.ReadFile(filepath.Join(req.Workspace, req.PrivatePaths[fluidui.PrivatePromptFile]))
	if err != nil {
		return harness.Outcome{}, err
	}
	a.received(string(prompt))
	out, err := a.FakeAdapter.Run(ctx, req)
	out.Transcript = append(append([]byte(nil), out.Transcript...), "the user asked: "+string(prompt)+"\n"...)
	out.Stderr = append(append([]byte(nil), out.Stderr...), "request: "+string(prompt)+"\n"...)
	return out, err
}

// fluidUIDaemon runs the shipped fluid-ui-customize workflow behind the
// ordinary trigger plane and the Labs request routes. propose answers each
// propose-stage call (1-based) with the candidate file body to write.
type fluidUIDaemon struct {
	root     string
	handler  http.Handler
	settle   func()
	calls    func() int
	received func() []string
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
	var received []string
	prevAdapter := runtimewiring.NewAgenticAdapter
	runtimewiring.NewAgenticAdapter = func(string, map[string]string) harness.Adapter {
		return fluidUIEchoingAgent{received: func(prompt string) {
			mu.Lock()
			defer mu.Unlock()
			received = append(received, prompt)
		}, FakeAdapter: &harnesstest.FakeAdapter{
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
				prompt, err := os.ReadFile(filepath.Join(req.Workspace, req.PrivatePaths[fluidui.PrivatePromptFile]))
				if err != nil {
					return err
				}
				return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "proposed for: " + string(prompt)})
			},
		}}
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
	reads, err := readservice.NewLocal(readservice.LocalSources{Layout: layout, Config: setup.Config, Definitions: setup.Definitions}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(reads, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return &fluidUIDaemon{
		root:    root,
		handler: handler,
		settle: func() {
			t.Helper()
			if err := service.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			sched.Wait()
			wg.Wait()
		},
		calls:    func() int { mu.Lock(); defer mu.Unlock(); return calls },
		received: func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(received) },
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

// get reads path as another view-role user and returns the status and body.
func (d *fluidUIDaemon) getAs(principal, path string) (int, string) {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set(fluidUIPrincipalHeader, principal)
	response := httptest.NewRecorder()
	d.handler.ServeHTTP(response, request)
	return response.Code, response.Body.String()
}

func TestFluidUIRequestPromptStaysOutOfTheRun(t *testing.T) {
	const prompt = "make the run list denser and hide the cost column"
	d := newFluidUIDaemon(t, func(int) string {
		return `{"version":"portal.customization/v1","density":"compact"}`
	})
	got := d.submitAndSettle(t, prompt)
	if got.Status != fluidui.StatusReadyToPreview || got.Prompt != prompt {
		t.Fatalf("requester's request = %+v", got)
	}
	if received := d.received(); len(received) != 1 || received[0] != prompt {
		t.Fatalf("agent received %q, want the request text once", received)
	}
	var list struct {
		Requests []fluidui.Request `json:"requests"`
	}
	d.call(t, http.MethodGet, apicontract.LabsFluidUIRequestsPath, "", http.StatusOK, &list)
	if len(list.Requests) != 1 || list.Requests[0].Prompt != prompt {
		t.Fatalf("requester's Feedback list = %+v", list)
	}

	const other = "another-viewer"
	if code, body := d.getAs(other, apicontract.LabsFluidUIRequestsPath+"/"+got.ID); code != http.StatusNotFound || strings.Contains(body, prompt) {
		t.Fatalf("another user reading the request = %d: %s", code, body)
	}
	run := strings.NewReplacer("{run}", got.RunID)
	read := func(path string) (int, string) {
		t.Helper()
		code, body := d.getAs(other, path)
		if strings.Contains(body, prompt) {
			t.Fatalf("%s shows the prompt to another user: %s", path, body)
		}
		return code, body
	}
	code, detail := read(run.Replace(apicontract.RunDetailPath))
	if code != http.StatusOK {
		t.Fatalf("run detail = %d: %s", code, detail)
	}
	code, events := read(run.Replace(apicontract.RunEventsPath))
	if code != http.StatusOK {
		t.Fatalf("run events = %d: %s", code, events)
	}
	loadResult, transcripts := false, 0
	for _, digest := range slices.Compact(slices.Sorted(slices.Values(regexp.MustCompile(`sha256:[0-9a-f]{64}`).FindAllString(detail+events, -1)))) {
		code, body := read(strings.NewReplacer("{run}", got.RunID, "{digest}", digest).Replace(apicontract.RunArtifactPath))
		loadResult = loadResult || code == http.StatusOK && strings.Contains(body, `"request":{"id":"`+got.ID+`"}`)
	}
	for _, seq := range regexp.MustCompile(`"seq":([0-9]+)`).FindAllStringSubmatch(events, -1) {
		path := strings.NewReplacer("{run}", got.RunID, "{seq}", seq[1]).Replace(apicontract.RunTranscriptPath)
		if code, body := read(path); code == http.StatusOK && strings.Contains(body, "the user asked: "+journal.Redacted) {
			transcripts++
		}
	}
	if !loadResult || transcripts == 0 {
		t.Fatalf("load-request result read: %v, redacted transcripts read: %d; the scan did not reach them", loadResult, transcripts)
	}

	private := filepath.Join(d.root, "labs", "fluid-ui")
	err := filepath.WalkDir(d.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || strings.HasPrefix(path, private+string(filepath.Separator)) {
			return err
		}
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), prompt) {
			t.Errorf("%s holds the prompt", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
