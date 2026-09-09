package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"

	"github.com/goobers/goobers/internal/cancelreceipt"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/workflow"
)

type engineCancelFixture struct {
	layout      instance.Layout
	service     *daemonCancelService
	definitions *interventionDefinitionRegistry
	temporal    *fakeEngineWorkflows
	lister      *fakeOpenWorkflowLister
	handler     http.Handler
	releases    int
}

func newEngineCancelFixture(t *testing.T, runID, workflowID string) *engineCancelFixture {
	t.Helper()
	f := &engineCancelFixture{layout: instance.NewLayout(t.TempDir())}
	// Remote mutations first inspect the authenticated inventory endpoint.
	// Give this active fixture a durable identity just like a provisioned daemon.
	writeFileContent(t, filepath.Join(f.layout.Root, instance.RootIdentityFileName), "0123456789abcdef0123456789abcdef\n")
	f.definitions = newInterventionDefinitionRegistry(cancelDefinitions("web"))
	f.temporal = &fakeEngineWorkflows{workflowIDs: map[string]string{workflowID: workflowID}}
	f.lister = &fakeOpenWorkflowLister{open: map[string]string{workflowID: "web"}}
	shared := &daemonEngineClient{client: &fakeTemporalClient{workflows: f.temporal, lister: f.lister}, namespace: "default"}
	instanceLog, _, err := journal.OpenInstanceLog(f.layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	f.service = newDaemonCancelService(newDaemonRunnerRegistry())
	f.service.engine = newDaemonEngineCancelService(f.layout, f.definitions, shared, shared.Guards(), instanceLog)
	f.service.AttachRelease(func(string, string) { f.releases++ })
	createDriverRun(t, f.layout.ForGaggle("web").RunsDir(), runID, "implementation", "web", journal.DriverEngine, time.Now(), nil)
	f.handler, err = httpapi.NewHandler(&engineCancelInventoryReader{layout: f.layout}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(engineCancelAuthenticator{}), httpapi.WithCancelService(f.service))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

type engineCancelInventoryReader struct {
	telemetryParityReader
	layout instance.Layout
}

func (r *engineCancelInventoryReader) Instance(context.Context) (readservice.Instance, error) {
	id, err := instance.ReadRootIdentity(r.layout.Root)
	if err != nil {
		return readservice.Instance{}, err
	}
	return readservice.Instance{
		InstanceRoot: r.layout.Root,
		RootIdentity: &readservice.RootIdentity{ID: id},
		Ready:        true,
	}, nil
}

func cancelDefinitions(gaggles ...string) interventionDefinitionSet {
	definitions := interventionDefinitionSet{machines: map[localscheduler.WorkflowIdentity]*workflow.Machine{}}
	for _, gaggle := range gaggles {
		definitions.machines[localscheduler.WorkflowIdentity{Gaggle: gaggle, Workflow: "implementation"}] = nil
	}
	return definitions
}

type engineCancelAuthenticator struct{}

func (engineCancelAuthenticator) Authenticate(r *http.Request) (*httpapi.Principal, error) {
	if r.Header.Get("Authorization") != "Bearer fixture-operator" {
		return nil, errors.New("fixture authentication refused")
	}
	return &httpapi.Principal{Subject: "authenticated-operator", Roles: []httpapi.Role{httpapi.RoleOperate}}, nil
}

func (f *engineCancelFixture) request(runID, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID+"/cancel", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "cancel-fixture")
	request.Header.Set("Authorization", "Bearer fixture-operator")
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

func TestDaemonEngineCancelHTTPDurableRetryDoesNotCancelTwice(t *testing.T) {
	f := newEngineCancelFixture(t, "engine-retry", "engine-retry")
	path := filepath.Join(f.layout.SchedulerDir(), "cancellation-receipts.db")
	store, err := cancelreceipt.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f.service.receipts, f.service.auditLog = store, f.service.engine.log
	first := f.request("engine-retry", `{"actor":"untrusted-body"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", first.Code, first.Body)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = cancelreceipt.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	f.service.receipts = store
	for i := 0; i < 5; i++ {
		again := f.request("engine-retry", `{"actor":"another-spoofed-actor"}`)
		if again.Code != http.StatusOK || again.Body.String() != first.Body.String() {
			t.Fatalf("retry status=%d body=%s want=%s", again.Code, again.Body, first.Body)
		}
	}
	conflict := f.request("engine-retry", `{"workflow":"another-workflow"}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body)
	}
	_, _, cancelled := f.temporal.snapshot()
	if len(cancelled) != 1 {
		t.Fatalf("duplicate engine calls=%v", cancelled)
	}
}

func TestDaemonEngineCancelHTTPRequestsWithoutTerminalizing(t *testing.T) {
	for _, scheduled := range []bool{false, true} {
		name, runID, workflowID := "direct", "engine-http-direct", "engine-http-direct"
		if scheduled {
			name, runID, workflowID = "scheduled", scheduledRunID(), testScheduleChildID
		}
		t.Run(name, func(t *testing.T) {
			f := newEngineCancelFixture(t, runID, workflowID)
			if err := f.layout.MigrateLegacyRuntime([]string{"web"}); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(f.layout.ForGaggle("web").RunsDir(), runID)
			before := cancellationRunFiles(t, dir)
			response := f.request(runID, `{"gaggle":"web","workflow":"implementation","actor":"untrusted-body"}`)
			if response.Code != http.StatusOK {
				t.Fatalf("cancel status %d: %s", response.Code, response.Body)
			}
			var result httpapi.CancelRunResult
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Code != httpapi.CancelCodeRequested || result.Phase != "" || result.Error != "" {
				t.Fatalf("cancel result = %+v", result)
			}
			_, _, cancelled := f.temporal.snapshot()
			wantCalls := 1
			if scheduled {
				wantCalls = 2
			}
			if len(cancelled) != wantCalls || cancelled[len(cancelled)-1] != workflowID {
				t.Fatalf("cancelled = %v, want %d calls ending %s", cancelled, wantCalls, workflowID)
			}
			if f.releases != 0 {
				t.Fatal("accepted request released scheduler capacity")
			}
			assertCancellationFilesUnchanged(t, before, cancellationRunFiles(t, dir))
			assertWatchdogPhase(t, f.layout.ForGaggle("web").RunsDir(), runID, journal.PhaseRunning)
			var announced bool
			for _, event := range instanceLogEvents(t, f.layout) {
				if event.Runner["action"] == journal.RecoveryActionEngineCancelRequested {
					announced = true
					if event.RunID != runID || event.Gaggle != "web" || event.Workflow != "implementation" || event.Runner["workflowId"] != workflowID || event.Runner["actor"] != "authenticated-operator" {
						t.Fatalf("bad cancellation audit: %+v", event)
					}
				}
			}
			if !announced {
				t.Fatal("no accepted cancellation audit")
			}
		})
	}
}

func cancellationRunFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func assertCancellationFilesUnchanged(t *testing.T, before, after map[string][]byte) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatal("cancellation changed journal files")
	}
	for name, data := range before {
		if !bytes.Equal(data, after[name]) {
			t.Fatalf("cancellation changed %s", name)
		}
	}
}

func TestDaemonEngineCancelRefusesUnownedOrMismatchedRun(t *testing.T) {
	cases := []struct {
		name, body, code string
		setup            func(*testing.T, *engineCancelFixture, string)
	}{
		{name: "wrong gaggle", body: `{"gaggle":"foreign"}`, code: "run_identity_mismatch"},
		{name: "wrong workflow", body: `{"workflow":"foreign"}`, code: "run_identity_mismatch"},
		{name: "removed definition", body: `{}`, code: "not_running", setup: func(_ *testing.T, f *engineCancelFixture, _ string) { f.definitions.Replace(cancelDefinitions()) }},
		{name: "legacy unowned", body: `{}`, code: "run_not_owned", setup: func(t *testing.T, f *engineCancelFixture, id string) {
			createDriverRun(t, f.layout.RunsDir(), id, "implementation", "foreign", journal.DriverEngine, time.Now(), nil)
			if err := os.RemoveAll(filepath.Join(f.layout.ForGaggle("web").RunsDir(), id)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "identity mismatch", body: `{}`, code: "run_identity_mismatch", setup: func(t *testing.T, f *engineCancelFixture, id string) {
			if err := os.RemoveAll(filepath.Join(f.layout.ForGaggle("web").RunsDir(), id)); err != nil {
				t.Fatal(err)
			}
			createDriverRun(t, f.layout.ForGaggle("web").RunsDir(), id, "implementation", "foreign", journal.DriverEngine, time.Now(), nil)
		}},
		{name: "ambiguous", body: `{"gaggle":"web"}`, code: "ambiguous_run_id", setup: func(t *testing.T, f *engineCancelFixture, id string) {
			f.definitions.Replace(cancelDefinitions("web", "other"))
			createDriverRun(t, f.layout.ForGaggle("other").RunsDir(), id, "implementation", "other", journal.DriverEngine, time.Now(), nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const id = "engine-refuse"
			f := newEngineCancelFixture(t, id, id)
			if tc.setup != nil {
				tc.setup(t, f, id)
			}
			response := f.request(id, tc.body)
			if (response.Code < 400 && tc.code != "not_running") || !strings.Contains(response.Body.String(), tc.code) {
				t.Fatalf("refusal %d: %s", response.Code, response.Body)
			}
			_, _, cancelled := f.temporal.snapshot()
			if len(cancelled) != 0 || f.releases != 0 {
				t.Fatalf("refused request touched engine or scheduler: %v/%d", cancelled, f.releases)
			}
		})
	}
}

func TestDaemonEngineCancelFailureDoesNotClaimAcceptance(t *testing.T) {
	for _, mode := range []string{"terminal", "missing client", "backend failure", "unresolvable", "foreign scheduled", "unauthenticated"} {
		t.Run(mode, func(t *testing.T) {
			id := scheduledRunID()
			f := newEngineCancelFixture(t, id, testScheduleChildID)
			switch mode {
			case "terminal":
				run, _, err := journal.Recover(filepath.Join(f.layout.ForGaggle("web").RunsDir(), id))
				if err != nil {
					t.Fatal(err)
				}
				if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
					t.Fatal(err)
				}
				if err := run.Close(); err != nil {
					t.Fatal(err)
				}
			case "missing client":
				f.service.engine.guards = nil
			case "backend failure":
				f.temporal.cancelErr = serviceerror.NewUnavailable("backend unavailable")
			case "unresolvable":
				f.lister.open = nil
			case "foreign scheduled":
				f.lister.open[testScheduleChildID] = "foreign"
			}
			dir := filepath.Join(f.layout.ForGaggle("web").RunsDir(), id)
			before := cancellationRunFiles(t, dir)
			var response *httptest.ResponseRecorder
			if mode == "unauthenticated" {
				request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+id+"/cancel", strings.NewReader(`{}`))
				request.Header.Set("Idempotency-Key", "key")
				response = httptest.NewRecorder()
				f.handler.ServeHTTP(response, request)
			} else {
				response = f.request(id, `{}`)
			}
			if mode == "terminal" {
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), httpapi.CancelCodeTerminal) {
					t.Fatalf("terminal: %d %s", response.Code, response.Body)
				}
			} else if response.Code < 400 {
				t.Fatalf("failure claimed success: %d %s", response.Code, response.Body)
			}
			if f.releases != 0 {
				t.Fatal("failure released scheduler capacity")
			}
			assertCancellationFilesUnchanged(t, before, cancellationRunFiles(t, dir))
			for _, event := range instanceLogEvents(t, f.layout) {
				if event.Runner["action"] == journal.RecoveryActionEngineCancelRequested {
					t.Fatal("failed cancellation recorded acceptance")
				}
			}
		})
	}
}

func TestDaemonEngineCancelUsesReloadedScheduledScope(t *testing.T) {
	id := scheduledRunID()
	f := newEngineCancelFixture(t, id, testScheduleChildID)
	// The live definition snapshot and actual engine memo both add a new gaggle
	// after service construction; a boot-only scope would refuse its inverse.
	f.definitions.Replace(cancelDefinitions("new"))
	f.lister.open[testScheduleChildID] = "new"
	createDriverRun(t, f.layout.ForGaggle("new").RunsDir(), id, "implementation", "new", journal.DriverEngine, time.Now(), nil)
	result, err := f.service.Cancel(context.Background(), httpapi.CancelRunRequest{RunID: id, Actor: "operator"})
	if err != nil || result.Code != httpapi.CancelCodeRequested {
		t.Fatalf("reloaded cancellation = %+v, %v", result, err)
	}
	_, _, cancelled := f.temporal.snapshot()
	if len(cancelled) != 2 || cancelled[1] != testScheduleChildID {
		t.Fatalf("reloaded inverse cancelled %v", cancelled)
	}
}

func TestDaemonEngineCancelUnknownPreservesNotRunning(t *testing.T) {
	f := newEngineCancelFixture(t, "known-engine-run", "known-engine-run")
	// This is the actual up.go composition, including an engine service even
	// for a daemon without an engine client.
	f.service.engine = newDaemonEngineCancelService(f.layout, f.definitions, nil, nil, nil)
	response := f.request("unknown-run", `{}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), httpapi.CancelCodeNotRunning) {
		t.Fatalf("unknown run compatibility: %d %s", response.Code, response.Body)
	}
}

func TestRunRemoteEngineCancelReportsOnlyRequest(t *testing.T) {
	for _, action := range []string{"cancel", "abort"} {
		t.Run(action, func(t *testing.T) {
			const id = "engine-cli-remote"
			f := newEngineCancelFixture(t, id, id)
			server := httptest.NewServer(f.handler)
			t.Cleanup(server.Close)
			t.Setenv(remoteDaemonAPIEnv, "")
			t.Setenv("GOOBERS_API_TOKEN", "fixture-operator")
			code, stdout, stderr := runArgs(t, "run", action, "--api", server.URL, id)
			if code != 0 || !strings.Contains(stdout, "requested cancellation") || strings.Contains(stdout, "aborted") {
				t.Fatalf("remote %s: code=%d stdout=%q stderr=%q", action, code, stdout, stderr)
			}
			identity, err := instance.ReadRootIdentity(f.layout.Root)
			if err != nil || !strings.Contains(stderr, "Remote instance root:") || !strings.Contains(stderr, identity) {
				t.Fatalf("remote identity was not displayed: stderr=%q identity error=%v", stderr, err)
			}
			_, _, cancelled := f.temporal.snapshot()
			if len(cancelled) != 1 || cancelled[0] != id {
				t.Fatalf("remote CLI did not reach engine cancellation: %v", cancelled)
			}
			if f.releases != 0 {
				t.Fatal("remote CLI request freed slot")
			}
		})
	}
}
