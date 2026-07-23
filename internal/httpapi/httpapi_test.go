package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/readservice"
)

type fakeReader struct {
	health       readservice.Health
	stats        readservice.TelemetryStatsResult
	errors       readservice.TelemetryErrorsPage
	telemetryErr error
	statsReq     readservice.TelemetryStatsRequest
	errorsReq    readservice.TelemetryErrorsRequest
	runs         readservice.RunList
	run          readservice.RunDetail
	events       readservice.EventList
	attempts     readservice.AttemptList
	artifact     readservice.ArtifactContent
	options      readservice.RunListOptions
	runID        string
	stage        string
	digest       string
	instance     readservice.Instance
	gaggles      readservice.GagglePage
	goobers      readservice.GooberPage
	workflows    readservice.WorkflowPage
	workflow     readservice.WorkflowDetail
	err          error
	called       int
	lastGaggle   string
	lastWorkflow string
	lastPage     readservice.PageRequest
}

type fakeAuthenticator struct {
	principal *Principal
	err       error
	called    int
}

func (f *fakeAuthenticator) Authenticate(*http.Request) (*Principal, error) {
	f.called++
	return f.principal, f.err
}

func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func (f *fakeReader) Health(context.Context) (readservice.Health, error) {
	f.called++
	return f.health, f.err
}

func (f *fakeReader) TelemetryStats(_ context.Context, req readservice.TelemetryStatsRequest) (readservice.TelemetryStatsResult, error) {
	f.statsReq = req
	return f.stats, f.telemetryErr
}

func (f *fakeReader) TelemetryErrors(_ context.Context, req readservice.TelemetryErrorsRequest) (readservice.TelemetryErrorsPage, error) {
	f.errorsReq = req
	return f.errors, f.telemetryErr
}

func (f *fakeReader) ListRuns(_ context.Context, options readservice.RunListOptions) (readservice.RunList, error) {
	f.options = options
	return f.runs, f.err
}

func (f *fakeReader) GetRun(_ context.Context, runID string) (readservice.RunDetail, error) {
	f.runID = runID
	return f.run, f.err
}

func (f *fakeReader) RunEvents(_ context.Context, runID string) (readservice.EventList, error) {
	f.runID = runID
	return f.events, f.err
}

func (f *fakeReader) StageAttempts(_ context.Context, runID, stage string) (readservice.AttemptList, error) {
	f.runID = runID
	f.stage = stage
	return f.attempts, f.err
}

func (f *fakeReader) Artifact(_ context.Context, runID, digest string) (readservice.ArtifactContent, error) {
	f.runID = runID
	f.digest = digest
	return f.artifact, f.err
}

func (f *fakeReader) Instance(context.Context) (readservice.Instance, error) {
	f.called++
	return f.instance, f.err
}

func (f *fakeReader) Gaggles(_ context.Context, page readservice.PageRequest) (readservice.GagglePage, error) {
	f.called++
	f.lastPage = page
	return f.gaggles, f.err
}

func (f *fakeReader) Goobers(_ context.Context, gaggle string, page readservice.PageRequest) (readservice.GooberPage, error) {
	f.called++
	f.lastGaggle = gaggle
	f.lastPage = page
	return f.goobers, f.err
}

func (f *fakeReader) Workflows(_ context.Context, gaggle string, page readservice.PageRequest) (readservice.WorkflowPage, error) {
	f.called++
	f.lastGaggle = gaggle
	f.lastPage = page
	return f.workflows, f.err
}

func (f *fakeReader) Workflow(_ context.Context, gaggle, workflow string) (readservice.WorkflowDetail, error) {
	f.called++
	f.lastGaggle = gaggle
	f.lastWorkflow = workflow
	return f.workflow, f.err
}

func TestHealthHandlerUsesSharedReadService(t *testing.T) {
	reader := &fakeReader{health: readservice.Health{
		APIVersion:    readservice.APIVersion,
		SchemaVersion: readservice.SchemaVersion,
		Ready:         true,
		Instance:      readservice.InstanceIdentity{Name: "example"},
	}}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, HealthPath, nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	var health readservice.Health
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if reader.called != 1 || !health.Ready || health.Instance.Name != "example" {
		t.Fatalf("reader called %d times, health = %+v", reader.called, health)
	}
}

func TestTierOneAuthenticatorDefaultsOpen(t *testing.T) {
	reader := &fakeReader{health: readservice.Health{Ready: true}}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, HealthPath, nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if reader.called != 1 {
		t.Fatalf("reader called %d times, want 1", reader.called)
	}
}

func TestAuthenticatorAcceptsAndRejectsBeforeAuthorization(t *testing.T) {
	t.Run("accepts and establishes principal", func(t *testing.T) {
		authenticator := &fakeAuthenticator{principal: &Principal{Subject: "user-1"}}
		authorized := false
		authorizer := authorizerFunc(func(request *http.Request) error {
			principal, ok := PrincipalFromRequest(request)
			if !ok || principal.Subject != "user-1" {
				t.Fatalf("principal = %+v, present = %t", principal, ok)
			}
			authorized = true
			return nil
		})
		handler, err := NewHandler(
			&fakeReader{health: readservice.Health{Ready: true}},
			authorizer,
			discardLogger(),
			WithAuthenticator(authenticator),
		)
		if err != nil {
			t.Fatal(err)
		}

		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, HealthPath, nil))

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
		if authenticator.called != 1 || !authorized {
			t.Fatalf("authenticator called %d times, authorized = %t", authenticator.called, authorized)
		}
	})

	t.Run("rejects before authorization", func(t *testing.T) {
		authenticator := &fakeAuthenticator{err: errors.New("invalid token")}
		authorized := false
		reader := &fakeReader{}
		handler, err := NewHandler(
			reader,
			authorizerFunc(func(*http.Request) error {
				authorized = true
				return nil
			}),
			discardLogger(),
			WithAuthenticator(authenticator),
		)
		if err != nil {
			t.Fatal(err)
		}

		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, HealthPath, nil))

		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
		var envelope ErrorEnvelope
		if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Error.Code != "unauthenticated" {
			t.Fatalf("error = %+v", envelope.Error)
		}
		if authenticator.called != 1 || authorized || reader.called != 0 {
			t.Fatalf(
				"authenticator called %d times, authorized = %t, reader called %d times",
				authenticator.called,
				authorized,
				reader.called,
			)
		}
	})
}

func TestRunDiagnosticRoutesUseSharedReadService(t *testing.T) {
	reader := &fakeReader{
		runs: readservice.RunList{Runs: []readservice.RunSummary{{ID: "run-1"}}},
		run: readservice.RunDetail{
			RunSummary:  readservice.RunSummary{ID: "run-1"},
			GraphStatus: "pinned",
		},
		events: readservice.EventList{RunID: "run-1", Events: []readservice.RunEvent{}},
		attempts: readservice.AttemptList{
			RunID:    "run-1",
			Stage:    "implement",
			Attempts: []readservice.StageAttempt{},
		},
		artifact: readservice.ArtifactContent{
			Metadata: readservice.ArtifactMetadata{
				Digest:    "sha256:abc",
				MediaType: "application/json",
			},
			Bytes: []byte(`{"ok":true}`),
		},
	}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
	}{
		{name: "list", path: RunsPath + "?workflow=implementation&gaggle=goobers&stage=implement&outcome=terminal&population=measured&phase=running&trigger=item&since=2026-07-01T00:00:00Z&until=2026-07-08T00:00:00Z&limit=10&cursor=next"},
		{name: "detail", path: RunsPath + "/run-1"},
		{name: "events", path: RunsPath + "/run-1/events"},
		{name: "attempts", path: RunsPath + "/run-1/stages/implement/attempts"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			if got := response.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q", got)
			}
		})
	}
	if reader.options.Workflow != "implementation" ||
		reader.options.Gaggle != "goobers" ||
		reader.options.Stage != "implement" ||
		reader.options.Outcome != readservice.OutcomeTerminal ||
		reader.options.StagePopulation != readservice.StagePopulationMeasured ||
		reader.options.Phase != "running" ||
		reader.options.Trigger != "item" ||
		reader.options.Limit != 10 ||
		reader.options.Cursor != "next" ||
		!reader.options.Since.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) ||
		!reader.options.Until.Equal(time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("list options = %+v", reader.options)
	}
	if reader.runID != "run-1" || reader.stage != "implement" {
		t.Fatalf("path values = run %q, stage %q", reader.runID, reader.stage)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, RunsPath+"/run-1/artifacts/sha256:abc", nil),
	)
	if response.Code != http.StatusOK || response.Body.String() != `{"ok":true}` {
		t.Fatalf("artifact response = %d %q", response.Code, response.Body.String())
	}
	if reader.runID != "run-1" || reader.digest != "sha256:abc" {
		t.Fatalf("artifact path values = run %q, digest %q", reader.runID, reader.digest)
	}
	if response.Header().Get("Content-Type") != "application/json" ||
		response.Header().Get("ETag") != `"sha256:abc"` ||
		response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("artifact headers = %+v", response.Header())
	}
}

func TestAPIErrorsUseStructuredEnvelope(t *testing.T) {
	tests := []struct {
		name       string
		reader     *fakeReader
		method     string
		path       string
		authorizer Authorizer
		wantStatus int
		wantCode   string
	}{
		{
			name:       "unknown route",
			reader:     &fakeReader{},
			method:     http.MethodGet,
			path:       Prefix + "/missing",
			authorizer: AllowAll,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "non-canonical route",
			reader:     &fakeReader{},
			method:     http.MethodGet,
			path:       Prefix + "/gaggles//workflows",
			authorizer: AllowAll,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "method",
			reader:     &fakeReader{},
			method:     http.MethodPost,
			path:       HealthPath,
			authorizer: AllowAll,
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "method_not_allowed",
		},
		{
			name:       "authorization",
			reader:     &fakeReader{},
			method:     http.MethodGet,
			path:       HealthPath,
			authorizer: authorizerFunc(func(*http.Request) error { return errors.New("denied") }),
			wantStatus: http.StatusForbidden,
			wantCode:   "forbidden",
		},
		{
			name:       "read error",
			reader:     &fakeReader{err: errors.New("disk failed")},
			method:     http.MethodGet,
			path:       HealthPath,
			authorizer: AllowAll,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "read_error",
		},
		{
			name:       "invalid list argument",
			reader:     &fakeReader{},
			method:     http.MethodGet,
			path:       RunsPath + "?limit=lots",
			authorizer: AllowAll,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_argument",
		},
		{
			name:       "invalid run time",
			reader:     &fakeReader{},
			method:     http.MethodGet,
			path:       RunsPath + "?since=yesterday",
			authorizer: AllowAll,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_argument",
		},
		{
			name:       "missing run",
			reader:     &fakeReader{err: readservice.ErrNotFound},
			method:     http.MethodGet,
			path:       RunsPath + "/missing",
			authorizer: AllowAll,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "artifact integrity",
			reader:     &fakeReader{err: readservice.ErrArtifactIntegrity},
			method:     http.MethodGet,
			path:       RunsPath + "/run-1/artifacts/sha256:abc",
			authorizer: AllowAll,
			wantStatus: http.StatusConflict,
			wantCode:   "artifact_invalid",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			handler, err := NewHandler(test.reader, test.authorizer, log.New(&logs, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			var envelope ErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != test.wantCode || envelope.Error.Message == "" {
				t.Fatalf("error = %+v", envelope.Error)
			}
			if test.wantCode == "read_error" && !strings.Contains(logs.String(), "disk failed") {
				t.Fatalf("server log = %q, want underlying read error", logs.String())
			}
		})
	}
}

func TestTelemetryHandlersUseSharedReadService(t *testing.T) {
	since := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	until := since.Add(2 * time.Hour)
	rate := 0.5
	reader := &fakeReader{
		stats: readservice.TelemetryStatsResult{
			Runs:   []readservice.TelemetryRunStats{{Workflow: "implement", TotalRuns: 2, SuccessRate: &rate}},
			Stages: []readservice.TelemetryStageStats{},
		},
		errors: readservice.TelemetryErrorsPage{
			Items:      []readservice.TelemetryError{{RunID: "run-1", Code: "failure"}},
			NextCursor: "next",
		},
	}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	statsResponse := httptest.NewRecorder()
	statsURL := TelemetryStatsPath + "?workflow=implement&gaggle=core&model=gpt-5.6-sol&harnessVersion=1.2.3&groupBy=model,harness-version&since=" +
		since.Format(time.RFC3339) + "&until=" + until.Format(time.RFC3339)
	handler.ServeHTTP(statsResponse, httptest.NewRequest(http.MethodGet, statsURL, nil))
	if statsResponse.Code != http.StatusOK {
		t.Fatalf("stats status = %d, body = %s", statsResponse.Code, statsResponse.Body)
	}
	wantStatsReq := readservice.TelemetryStatsRequest{
		Workflow:              "implement",
		Gaggle:                "core",
		Model:                 "gpt-5.6-sol",
		HarnessVersion:        "1.2.3",
		GroupByModel:          true,
		GroupByHarnessVersion: true,
		Since:                 since,
		Until:                 until,
	}
	if reader.statsReq != wantStatsReq {
		t.Fatalf("stats request = %+v, want %+v", reader.statsReq, wantStatsReq)
	}
	var stats readservice.TelemetryStatsResult
	if err := json.NewDecoder(statsResponse.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if len(stats.Runs) != 1 || stats.Runs[0].Workflow != "implement" {
		t.Fatalf("stats = %+v", stats)
	}

	errorsResponse := httptest.NewRecorder()
	errorsURL := TelemetryErrorsPath + "?workflow=implement&gaggle=core&class=timeout&limit=10&cursor=current"
	handler.ServeHTTP(errorsResponse, httptest.NewRequest(http.MethodGet, errorsURL, nil))
	if errorsResponse.Code != http.StatusOK {
		t.Fatalf("errors status = %d, body = %s", errorsResponse.Code, errorsResponse.Body)
	}
	wantErrorsReq := readservice.TelemetryErrorsRequest{
		Workflow:   "implement",
		Gaggle:     "core",
		ErrorClass: "timeout",
		Limit:      10,
		Cursor:     "current",
	}
	if reader.errorsReq != wantErrorsReq {
		t.Fatalf("errors request = %+v, want %+v", reader.errorsReq, wantErrorsReq)
	}
	var page readservice.TelemetryErrorsPage
	if err := json.NewDecoder(errorsResponse.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Code != "failure" || page.NextCursor != "next" {
		t.Fatalf("errors page = %+v", page)
	}
}

func TestTelemetryQueryErrorsAreStructured(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "invalid time", path: TelemetryStatsPath + "?since=yesterday"},
		{name: "reversed window", path: TelemetryStatsPath + "?since=2026-07-02T00:00:00Z&until=2026-07-01T00:00:00Z"},
		{name: "unknown parameter", path: TelemetryStatsPath + "?sort=recent"},
		{name: "duplicate parameter", path: TelemetryStatsPath + "?workflow=a&workflow=b"},
		{name: "invalid limit", path: TelemetryErrorsPath + "?limit=0"},
		{name: "oversized limit", path: TelemetryErrorsPath + "?limit=201"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger())
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			var envelope ErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != "invalid_query" || envelope.Error.Message == "" {
				t.Fatalf("error = %+v", envelope.Error)
			}
		})
	}
}

func TestTelemetryReadErrorsAreStructured(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "invalid cursor",
			err:        readservice.ErrInvalidTelemetryRequest,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_query",
		},
		{
			name:       "disabled",
			err:        readservice.ErrTelemetryUnavailable,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "telemetry_unavailable",
		},
		{
			name:       "storage",
			err:        errors.New("sqlite failed"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "read_error",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			handler, err := NewHandler(
				&fakeReader{telemetryErr: test.err},
				AllowAll,
				log.New(&logs, "", 0),
			)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, TelemetryErrorsPath, nil))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			var envelope ErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != test.wantCode {
				t.Fatalf("error = %+v", envelope.Error)
			}
			if test.wantCode == "read_error" && !strings.Contains(logs.String(), "sqlite failed") {
				t.Fatalf("server log = %q", logs.String())
			}
		})
	}
}

func TestServerLifecycleAndStartupFailure(t *testing.T) {
	handler, err := NewHandler(&fakeReader{health: readservice.Health{Ready: true}}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer("127.0.0.1:0", handler, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get("http://" + server.Address() + HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-server.Errors(); ok {
		t.Fatal("errors channel should close after graceful shutdown")
	}

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := occupied.Close(); err != nil {
			t.Errorf("close occupied listener: %v", err)
		}
	})
	blocked, err := NewServer(occupied.Addr().String(), handler, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := blocked.Start(); err == nil {
		t.Fatal("expected occupied listener startup to fail")
	}
}

func TestServerReadTimeoutOption(t *testing.T) {
	const timeout = 3 * time.Second
	server, err := NewServer("127.0.0.1:0", http.NotFoundHandler(), discardLogger(), WithReadTimeout(timeout))
	if err != nil {
		t.Fatal(err)
	}
	if server.http.ReadTimeout != timeout {
		t.Fatalf("ReadTimeout = %s, want %s", server.http.ReadTimeout, timeout)
	}
	if _, err := NewServer("127.0.0.1:0", http.NotFoundHandler(), discardLogger(), WithReadTimeout(0)); err == nil {
		t.Fatal("expected non-positive read timeout error")
	}
}

func TestConstructorsRequireDependencies(t *testing.T) {
	if _, err := NewHandler(nil, AllowAll, discardLogger()); err == nil {
		t.Fatal("expected missing reader error")
	}
	if _, err := NewHandler(&fakeReader{}, nil, discardLogger()); err == nil {
		t.Fatal("expected missing authorizer error")
	}
	if _, err := NewHandler(&fakeReader{}, AllowAll, nil); err == nil {
		t.Fatal("expected missing error logger error")
	}
	if _, err := NewHandler(
		&fakeReader{},
		AllowAll,
		discardLogger(),
		WithAuthenticator(nil),
	); err == nil {
		t.Fatal("expected missing authenticator error")
	}
	if _, err := NewServer("", http.NotFoundHandler(), discardLogger()); err == nil {
		t.Fatal("expected missing address error")
	}
	if _, err := NewServer("127.0.0.1:0", nil, discardLogger()); err == nil {
		t.Fatal("expected missing handler error")
	}
	if _, err := NewServer("127.0.0.1:0", http.NotFoundHandler(), nil); err == nil {
		t.Fatal("expected missing error logger error")
	}
	if _, err := NewServer("127.0.0.1:0", http.NotFoundHandler(), discardLogger(), nil); err == nil {
		t.Fatal("expected missing server option error")
	}
}
