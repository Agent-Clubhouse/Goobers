package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/fluidui"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

const fluidUITestRunID = "0123456789abcdef0123456789abcdef"

type fluidUIRequestFixture struct {
	store    *fluidui.Store
	requests *fluidui.Requests
	runs     map[string]fluidui.RunState
	triggers *fakeTriggerService
}

func newFluidUIRequestFixture(t *testing.T) *fluidUIRequestFixture {
	t.Helper()
	f := &fluidUIRequestFixture{
		store:    fluidui.NewStore(t.TempDir()),
		runs:     map[string]fluidui.RunState{},
		triggers: &fakeTriggerService{response: TriggerResponse{AcceptanceID: "trigger-" + fluidUITestRunID, State: "accepted"}},
	}
	f.requests = fluidui.NewRequests(f.store, func(runID string) (fluidui.RunState, error) { return f.runs[runID], nil })
	return f
}

func (f *fluidUIRequestFixture) handler(t *testing.T, auth *fakeAuthenticator, withTriggers bool) http.Handler {
	t.Helper()
	opts := []HandlerOption{WithLabsFluidUI(f.store), WithLabsFluidUIRequests(f.requests)}
	if withTriggers {
		opts = append(opts, WithTriggerService(f.triggers))
	}
	authorizer := AllowAll
	if auth != nil {
		opts = append(opts, WithAuthenticator(auth))
		authorizer = RequireRoles()
	}
	handler, err := NewHandler(&fakeReader{}, authorizer, discardLogger(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func fluidUICall(t *testing.T, handler http.Handler, method, path, body string, out any) (int, string) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if out != nil && response.Code < 300 {
		if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}
	return response.Code, response.Body.String()
}

func fluidUIRequestPath(id, action string) string {
	path := apicontract.LabsFluidUIRequestsPath + "/" + id
	if action != "" {
		path += "/" + action
	}
	return path
}

func TestLabsFluidUIRequestLifecycleThroughHandlers(t *testing.T) {
	f := newFluidUIRequestFixture(t)
	alice := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "alice", Roles: []Role{RoleView}}}
	handler := f.handler(t, alice, true)

	var submitted fluidui.Request
	if code, body := fluidUICall(t, handler, http.MethodPost, apicontract.LabsFluidUIRequestsPath, `{"prompt":"make it compact"}`, &submitted); code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", code, body)
	}
	if submitted.Status != fluidui.StatusQueued || submitted.RunID != fluidUITestRunID {
		t.Fatalf("submitted = %+v", submitted)
	}
	if len(f.triggers.requests) != 1 || f.triggers.requests[0].Workflow != fluidui.Workflow || f.triggers.requests[0].Actor != "alice" {
		t.Fatalf("trigger requests = %+v", f.triggers.requests)
	}
	if strings.Contains(f.triggers.requests[0].RequestID, "compact") {
		t.Fatalf("trigger key carries the prompt: %q", f.triggers.requests[0].RequestID)
	}

	var got fluidui.Request
	if code, _ := fluidUICall(t, handler, http.MethodPost, fluidUIRequestPath(submitted.ID, "apply"), "", nil); code != http.StatusConflict {
		t.Fatalf("apply while queued = %d, want 409", code)
	}
	f.runs[fluidUITestRunID] = fluidui.RunState{Phase: journal.PhaseRunning}
	if fluidUICall(t, handler, http.MethodGet, fluidUIRequestPath(submitted.ID, ""), "", &got); got.Status != fluidui.StatusImplementing {
		t.Fatalf("running request = %+v", got)
	}
	result := fluidui.EvaluateCandidate([]byte(`{"version":"portal.customization/v1","density":"compact"}`))
	f.runs[fluidUITestRunID] = fluidui.RunState{Phase: journal.PhaseCompleted, Result: &result}
	var list labsFluidUIRequestList
	if code, _ := fluidUICall(t, handler, http.MethodGet, apicontract.LabsFluidUIRequestsPath, "", &list); code != http.StatusOK || len(list.Requests) != 1 || list.Requests[0].Status != fluidui.StatusReadyToPreview {
		t.Fatalf("list = %d %+v", code, list)
	}
	if state, _ := f.store.Get("oidc:alice"); state.Customization != nil {
		t.Fatalf("a ready candidate was applied without the user: %+v", state)
	}

	var applied labsFluidUIApplication
	if code, body := fluidUICall(t, handler, http.MethodPost, fluidUIRequestPath(submitted.ID, "apply"), "{}", &applied); code != http.StatusOK {
		t.Fatalf("apply = %d %s", code, body)
	}
	if applied.Request.Status != fluidui.StatusApplied || applied.Customization.Customization == nil || applied.Customization.Customization.Density != fluidui.DensityCompact {
		t.Fatalf("applied = %+v", applied)
	}
	if code, body := fluidUICall(t, handler, http.MethodPost, fluidUIRequestPath(submitted.ID, "revert"), "", &applied); code != http.StatusOK || applied.Request.Status != fluidui.StatusReverted || applied.Customization.Customization != nil {
		t.Fatalf("revert = %d %s", code, body)
	}
}

func TestLabsFluidUIRequestsArePrivateAndAuthorized(t *testing.T) {
	f := newFluidUIRequestFixture(t)
	alice := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "alice", Roles: []Role{RoleView}}}
	bob := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "bob", Roles: []Role{RoleView}}}

	var submitted fluidui.Request
	if code, body := fluidUICall(t, f.handler(t, alice, true), http.MethodPost, apicontract.LabsFluidUIRequestsPath, `{"prompt":"hide the activity panel"}`, &submitted); code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", code, body)
	}

	bobHandler := f.handler(t, bob, true)
	for _, action := range []string{"", "retry", "apply", "revert"} {
		method := http.MethodPost
		if action == "" {
			method = http.MethodGet
		}
		if code, body := fluidUICall(t, bobHandler, method, fluidUIRequestPath(submitted.ID, action), "", nil); code != http.StatusNotFound || strings.Contains(body, "activity") {
			t.Fatalf("bob %s %q = %d %s, want 404", method, action, code, body)
		}
	}
	var list labsFluidUIRequestList
	if fluidUICall(t, bobHandler, http.MethodGet, apicontract.LabsFluidUIRequestsPath, "", &list); len(list.Requests) != 0 {
		t.Fatalf("bob lists alice's requests: %+v", list)
	}

	pod := &fakeAuthenticator{principal: &Principal{Issuer: PodPrincipalIssuer, Subject: "run-1"}}
	if code, _ := fluidUICall(t, f.handler(t, pod, true), http.MethodPost, apicontract.LabsFluidUIRequestsPath, `{"prompt":"x"}`, nil); code != http.StatusForbidden {
		t.Fatalf("pod submit = %d, want 403", code)
	}
	noRole := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "carol"}}
	if code, _ := fluidUICall(t, f.handler(t, noRole, true), http.MethodPost, apicontract.LabsFluidUIRequestsPath, `{"prompt":"x"}`, nil); code != http.StatusForbidden {
		t.Fatalf("role-less submit = %d, want 403", code)
	}
}

const fluidUIPrivatePrompt = "make the \"Runs\" panel <compact> & dense\nthen hide the activity feed"

// fluidUIPrivateRunReader serves one Fluid UI run whose transcript, artifact,
// and agent progress all restate fluidUIPrivatePrompt, including the
// doubly-escaped form a JSONL transcript embeds tool output in.
func fluidUIPrivateRunReader(t *testing.T, runID string) *fakeReader {
	t.Helper()
	encoded, err := json.Marshal(fluidUIPrivatePrompt)
	if err != nil {
		t.Fatal(err)
	}
	toolOutput, err := json.Marshal(`{"request":{"text":` + string(encoded) + `}}`)
	if err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"tool.result","content":` + string(toolOutput) + "}\n" +
		`{"type":"assistant.message","content":"The user asked to make the Runs panel compact"}` + "\n"
	progress := &journal.AgentProgress{
		Schema: "goobers.dev/journal/agent-progress/v1", AgentID: "agent-1", RunID: runID, Stage: "propose",
		Attempt: 1, Sequence: 1, Kind: "status", Summary: "Working on: " + fluidUIPrivatePrompt,
		Plan: []string{fluidUIPrivatePrompt},
	}
	latestError := &journal.ErrorDetail{Code: "agent_failed", Message: fluidUIPrivatePrompt}
	summary := readservice.RunSummary{
		ID: runID, Workflow: fluidui.Workflow, TerminalReason: "failed on " + fluidUIPrivatePrompt,
		Operator: readservice.OperatorRunSummary{
			LatestError:       latestError,
			Review:            &readservice.OperatorReview{Verdict: "fail", Rationale: fluidUIPrivatePrompt},
			PotentialBlockers: []string{"agent_failed: " + fluidUIPrivatePrompt},
			Blockers:          []readservice.OperatorBlocker{{Code: "agent_failed", Source: "error", Message: "agent_failed: " + fluidUIPrivatePrompt}},
		},
	}
	return &fakeReader{
		runs: readservice.RunList{Runs: []readservice.RunSummary{summary}},
		errors: readservice.TelemetryErrorsPage{Items: []readservice.TelemetryError{
			{RunID: runID, Workflow: fluidui.Workflow, Stage: "propose", Attempt: 1, Code: "agent_failed", Message: fluidUIPrivatePrompt},
		}},
		run: readservice.RunDetail{
			RunSummary:    summary,
			AgentProgress: []readservice.AgentProgressSummary{{AgentID: "agent-1", RunID: runID, Stage: "propose", Latest: progress, History: []journal.AgentProgress{*progress}}},
		},
		events: readservice.EventList{RunID: runID, Events: []readservice.RunEvent{
			{Seq: 1, Type: journal.EventRunStarted, Workflow: fluidui.Workflow},
			{Seq: 2, Type: journal.EventAgentProgress, Stage: "propose", Progress: progress},
			{Seq: 3, Type: journal.EventStageFinished, Stage: "propose", Error: latestError},
		}},
		attempts: readservice.AttemptList{RunID: runID, Stage: "propose", Attempts: []readservice.StageAttempt{
			{ID: "a1", Number: 1, Error: latestError},
		}},
		artifact: readservice.ArtifactContent{
			Metadata: readservice.ArtifactMetadata{Digest: "sha256:prompt", MediaType: "application/json"},
			Bytes:    []byte(`{"request":{"text":` + string(encoded) + `}}`),
		},
		transcript: readservice.TranscriptContent{Seq: 1, Stage: "propose", Name: "transcript", Bytes: []byte(transcript)},
	}
}

func fluidUIRunPaths(runID string) map[string]string {
	run := strings.ReplaceAll(apicontract.RunDetailPath, "{run}", runID)
	return map[string]string{
		"runs":       apicontract.RunsPath,
		"errors":     apicontract.TelemetryErrorsPath,
		"detail":     run,
		"events":     strings.ReplaceAll(apicontract.RunEventsPath, "{run}", runID),
		"attempts":   strings.ReplaceAll(strings.ReplaceAll(apicontract.StageAttemptsPath, "{run}", runID), "{stage}", "propose"),
		"artifact":   strings.ReplaceAll(strings.ReplaceAll(apicontract.RunArtifactPath, "{run}", runID), "{digest}", "sha256:prompt"),
		"transcript": strings.ReplaceAll(strings.ReplaceAll(apicontract.RunTranscriptPath, "{run}", runID), "{seq}", "1"),
	}
}

// fluidUIPromptFragments are pieces of the prompt in every encoding the run's
// content carries it; none may reach a viewer the run is private from.
var fluidUIPromptFragments = []string{"Runs", "compact", "activity feed", `\u003c`, `\\\"`}

func assertFluidUIRunContent(t *testing.T, handler http.Handler, runID, who string, visible bool) {
	t.Helper()
	for name, path := range fluidUIRunPaths(runID) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		body := response.Body.String()
		if response.Header().Get("Cache-Control") != "private, no-store" {
			t.Errorf("%s %s cache control = %q", who, name, response.Header().Get("Cache-Control"))
		}
		if visible {
			if response.Code != http.StatusOK || !strings.Contains(body, "compact") {
				t.Errorf("%s %s = %d %q, want the prompt", who, name, response.Code, body)
			}
			continue
		}
		wantCode := http.StatusOK
		if name == "artifact" || name == "transcript" {
			wantCode = http.StatusForbidden
		}
		if response.Code != wantCode {
			t.Errorf("%s %s = %d %q, want %d", who, name, response.Code, body, wantCode)
		}
		for _, fragment := range fluidUIPromptFragments {
			if strings.Contains(body, fragment) {
				t.Errorf("%s %s leaks %q: %s", who, name, fragment, body)
			}
		}
	}
}

func newFluidUIPrivateRunHandler(t *testing.T, f *fluidUIRequestFixture, reader *fakeReader, auth *fakeAuthenticator) http.Handler {
	t.Helper()
	handler, err := NewHandler(reader, RequireRoles(), discardLogger(),
		WithAuthenticator(auth), WithLabsFluidUI(f.store), WithLabsFluidUIRequests(f.requests), WithTriggerService(f.triggers))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestLabsFluidUIRunContentIsPrivateToRequesterAndAdmins(t *testing.T) {
	f := newFluidUIRequestFixture(t)
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "alice", Roles: []Role{RoleView}}}
	handler := newFluidUIPrivateRunHandler(t, f, fluidUIPrivateRunReader(t, fluidUITestRunID), auth)

	var submitted fluidui.Request
	body, _ := json.Marshal(map[string]string{"prompt": fluidUIPrivatePrompt})
	if code, out := fluidUICall(t, handler, http.MethodPost, apicontract.LabsFluidUIRequestsPath, string(body), &submitted); code != http.StatusAccepted || submitted.RunID != fluidUITestRunID {
		t.Fatalf("submit = %d %s", code, out)
	}
	assertFluidUIRunContent(t, handler, fluidUITestRunID, "requester", true)

	auth.principal = &Principal{Issuer: "oidc", Subject: "bob", Roles: []Role{RoleView}}
	assertFluidUIRunContent(t, handler, fluidUITestRunID, "other viewer", false)

	auth.principal = &Principal{Issuer: "oidc", Subject: "bob", Roles: []Role{RoleAdmin}}
	assertFluidUIRunContent(t, handler, fluidUITestRunID, "admin", true)

	var ownerRequest fluidui.Request
	auth.principal = &Principal{Issuer: "oidc", Subject: "alice", Roles: []Role{RoleView}}
	if code, out := fluidUICall(t, handler, http.MethodGet, fluidUIRequestPath(submitted.ID, ""), "", &ownerRequest); code != http.StatusOK || ownerRequest.Prompt != fluidUIPrivatePrompt {
		t.Fatalf("owner feedback request = %d %+v %s", code, ownerRequest, out)
	}
}

func TestLabsFluidUIRunWithoutBindingIsPrivateToAdmins(t *testing.T) {
	const unbound = "fedcba9876543210fedcba9876543210"
	f := newFluidUIRequestFixture(t)
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "alice", Roles: []Role{RoleView}}}
	handler := newFluidUIPrivateRunHandler(t, f, fluidUIPrivateRunReader(t, unbound), auth)

	assertFluidUIRunContent(t, handler, unbound, "non-admin", false)
	auth.principal = &Principal{Issuer: "oidc", Subject: "alice", Roles: []Role{RoleAdmin}}
	assertFluidUIRunContent(t, handler, unbound, "admin", true)
}

func TestRunContentOfOtherWorkflowsStaysReadable(t *testing.T) {
	const runID = "fedcba9876543210fedcba9876543210"
	f := newFluidUIRequestFixture(t)
	reader := fluidUIPrivateRunReader(t, runID)
	reader.run.Workflow = "implement"
	reader.runs.Runs[0].Workflow = "implement"
	reader.errors.Items[0].Workflow = "implement"
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "oidc", Subject: "bob", Roles: []Role{RoleView}}}
	assertFluidUIRunContent(t, newFluidUIPrivateRunHandler(t, f, reader, auth), runID, "viewer", true)
}

func TestLabsFluidUIRequestRejectsBadInputAndMissingTriggers(t *testing.T) {
	f := newFluidUIRequestFixture(t)
	handler := f.handler(t, nil, true)
	for name, body := range map[string]string{
		"unknown field": `{"prompt":"x","workflow":"other"}`,
		"empty prompt":  `{"prompt":"   "}`,
		"too short":     `{"prompt":"dark"}`,
		"not json":      `make it blue`,
		"too long":      `{"prompt":"` + strings.Repeat("a", fluidui.MaxPromptBytes+1) + `"}`,
	} {
		if code, _ := fluidUICall(t, handler, http.MethodPost, apicontract.LabsFluidUIRequestsPath, body, nil); code != http.StatusBadRequest {
			t.Fatalf("%s submit = %d, want 400", name, code)
		}
	}
	request := httptest.NewRequest(http.MethodPost, apicontract.LabsFluidUIRequestsPath, strings.NewReader(`{"prompt":"x"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("submit without JSON content type = %d", response.Code)
	}
	if len(f.triggers.requests) != 0 {
		t.Fatalf("rejected submissions reached the trigger plane: %+v", f.triggers.requests)
	}

	if code, _ := fluidUICall(t, f.handler(t, nil, false), http.MethodPost, apicontract.LabsFluidUIRequestsPath, `{"prompt":"make it compact"}`, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("submit without triggers = %d, want 503", code)
	}
}
