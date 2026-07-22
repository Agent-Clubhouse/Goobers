package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
)

func TestInventoryHandlersUseSharedReader(t *testing.T) {
	reader := &fakeReader{
		instance: readservice.Instance{Name: "clubhouse"},
		gaggles: readservice.GagglePage{
			Items: []readservice.Gaggle{{Name: "alpha"}},
			Page:  readservice.PageInfo{Limit: 2, Total: 1},
		},
		goobers: readservice.GooberPage{Items: []readservice.Goober{{Name: "builder"}}},
		workflows: readservice.WorkflowPage{
			Items: []readservice.WorkflowSummary{{
				Identity: readservice.WorkflowReference{Gaggle: "alpha", Name: "deploy"},
			}},
		},
		workflow: readservice.WorkflowDetail{WorkflowSummary: readservice.WorkflowSummary{
			Identity: readservice.WorkflowReference{Gaggle: "alpha", Name: "deploy"},
		}},
	}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, InstancePath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("instance status = %d, body = %s", response.Code, response.Body)
	}
	var instanceView readservice.Instance
	if err := json.NewDecoder(response.Body).Decode(&instanceView); err != nil {
		t.Fatal(err)
	}
	if instanceView.Name != "clubhouse" {
		t.Fatalf("instance = %+v", instanceView)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, GagglesPath+"?limit=2&cursor=opaque", nil))
	if response.Code != http.StatusOK || reader.lastPage != (readservice.PageRequest{Limit: 2, Cursor: "opaque"}) {
		t.Fatalf("gaggles status/page = %d / %+v, body = %s", response.Code, reader.lastPage, response.Body)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, Prefix+"/gaggles/alpha/goobers", nil))
	if response.Code != http.StatusOK || reader.lastGaggle != "alpha" {
		t.Fatalf("goobers status/gaggle = %d / %q, body = %s", response.Code, reader.lastGaggle, response.Body)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, Prefix+"/gaggles/alpha/workflows", nil))
	if response.Code != http.StatusOK || reader.lastGaggle != "alpha" {
		t.Fatalf("workflows status/gaggle = %d / %q, body = %s", response.Code, reader.lastGaggle, response.Body)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, Prefix+"/gaggles/alpha/workflows/deploy", nil))
	if response.Code != http.StatusOK || reader.lastGaggle != "alpha" || reader.lastWorkflow != "deploy" {
		t.Fatalf("detail status/identity = %d / %q/%q, body = %s",
			response.Code, reader.lastGaggle, reader.lastWorkflow, response.Body)
	}
}

func TestInventoryHandlersAcceptConfiguredIdentifierLength(t *testing.T) {
	gaggle := strings.Repeat("a", 254)
	reader := &fakeReader{goobers: readservice.GooberPage{Items: []readservice.Goober{{Name: "builder"}}}}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, Prefix+"/gaggles/"+gaggle+"/goobers", nil))
	if response.Code != http.StatusOK || reader.lastGaggle != gaggle {
		t.Fatalf("status/gaggle = %d / %q, body = %s", response.Code, reader.lastGaggle, response.Body)
	}
}

func TestWorkflowEndpointsLoadScopedProductionDefinitionsAndWarnings(t *testing.T) {
	definitions, report, err := instance.LoadConfigDir("testdata/scoped-inventory")
	if err != nil {
		t.Fatalf("LoadConfigDir: %v", err)
	}
	if len(definitions.Workflows) != 2 {
		t.Fatalf("loaded workflows = %+v", definitions.Workflows)
	}
	reads, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      instance.NewLayout(t.TempDir()),
		Definitions: definitions,
		Validation:  report,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	handler, err := NewHandler(reads, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	readWorkflow := func(gaggle string) readservice.WorkflowDetail {
		t.Helper()
		response := httptest.NewRecorder()
		path := Prefix + "/gaggles/" + gaggle + "/workflows/deploy"
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", gaggle, response.Code, response.Body)
		}
		var detail readservice.WorkflowDetail
		if err := json.NewDecoder(response.Body).Decode(&detail); err != nil {
			t.Fatal(err)
		}
		return detail
	}

	alpha := readWorkflow("alpha")
	beta := readWorkflow("beta")
	if alpha.Identity != (readservice.WorkflowReference{Gaggle: "alpha", Name: "deploy"}) ||
		beta.Identity != (readservice.WorkflowReference{Gaggle: "beta", Name: "deploy"}) {
		t.Fatalf("workflow identities = %+v / %+v", alpha.Identity, beta.Identity)
	}
	var compatibilityWarnings []validate.CodedWarning
	previewWarnings := 0
	for _, warning := range alpha.Warnings {
		switch warning.Code {
		case validate.WarningCompatibility:
			compatibilityWarnings = append(compatibilityWarnings, warning)
		case validate.WarningPreviewFeature:
			previewWarnings++
		}
	}
	if previewWarnings != 0 {
		t.Fatalf("alpha warnings must contain no preview notices — standard DSL fields are GA (#1196): %+v", alpha.Warnings)
	}
	if len(compatibilityWarnings) != 2 {
		t.Fatalf("alpha compatibility warnings = %+v", compatibilityWarnings)
	}
	claimWarning := compatibilityWarnings[0]
	if claimWarning.Code != validate.WarningCompatibility || claimWarning.Severity != validate.Warning ||
		claimWarning.Scope != "gaggles/alpha/workflows/deploy.yaml Gaggle/alpha Workflow/deploy" ||
		!strings.Contains(claimWarning.Explanation, "inputs.resultFile") {
		t.Fatalf("alpha claim warning = %+v", claimWarning)
	}
	triggerWarning := compatibilityWarnings[1]
	if triggerWarning.Code != validate.WarningCompatibility || triggerWarning.Severity != validate.Warning ||
		triggerWarning.Scope != "gaggles/alpha/workflows/deploy.yaml Gaggle/alpha Workflow/deploy" ||
		!strings.Contains(triggerWarning.Explanation, "has no schedule trigger") {
		t.Fatalf("alpha trigger warning = %+v", triggerWarning)
	}
	var betaCompatibilityWarnings []validate.CodedWarning
	betaPreviewWarnings := 0
	for _, warning := range beta.Warnings {
		switch warning.Code {
		case validate.WarningCompatibility:
			betaCompatibilityWarnings = append(betaCompatibilityWarnings, warning)
		case validate.WarningPreviewFeature:
			betaPreviewWarnings++
		}
	}
	if betaPreviewWarnings != 0 || len(betaCompatibilityWarnings) != 1 {
		t.Fatalf("beta warnings must be a single compatibility notice with no preview notices — GA (#1196): %+v", beta.Warnings)
	}
	betaTriggerWarning := betaCompatibilityWarnings[0]
	if betaTriggerWarning.Code != validate.WarningCompatibility || betaTriggerWarning.Severity != validate.Warning ||
		betaTriggerWarning.Scope != "gaggles/beta/workflows/deploy.yaml Gaggle/beta Workflow/deploy" ||
		!strings.Contains(betaTriggerWarning.Explanation, "has no schedule trigger") {
		t.Fatalf("beta trigger warning = %+v", betaTriggerWarning)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, Prefix+"/gaggles/alpha/workflows", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("alpha list status = %d, body = %s", response.Code, response.Body)
	}
	var page readservice.WorkflowPage
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || len(page.Items[0].Warnings) != len(alpha.Warnings) {
		t.Fatalf("alpha list warnings = %+v", page.Items)
	}
	for i := range alpha.Warnings {
		if page.Items[0].Warnings[i] != alpha.Warnings[i] {
			t.Fatalf("alpha list warning %d = %+v, want %+v", i, page.Items[0].Warnings[i], alpha.Warnings[i])
		}
	}
}

func TestInventoryHandlerErrorsUseStandardEnvelope(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		readerErr  error
		wantStatus int
		wantCode   string
		wantCalled bool
	}{
		{
			name:       "invalid gaggle identifier",
			path:       Prefix + "/gaggles/Bad_Name/goobers",
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_identifier",
		},
		{
			name:       "invalid workflow identifier",
			path:       Prefix + "/gaggles/alpha/workflows/Bad_Name",
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_identifier",
		},
		{
			name:       "invalid cursor",
			path:       Prefix + "/gaggles/alpha/workflows?cursor=malformed",
			readerErr:  readservice.ErrInvalidCursor,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_cursor",
			wantCalled: true,
		},
		{
			name:       "malformed cursor query",
			path:       Prefix + "/gaggles/alpha/workflows?cursor=bad;value",
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_cursor",
		},
		{
			name:       "invalid limit",
			path:       GagglesPath + "?limit=0",
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_pagination",
		},
		{
			name:       "missing workflow",
			path:       Prefix + "/gaggles/alpha/workflows/missing",
			readerErr:  readservice.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
			wantCalled: true,
		},
		{
			name:       "read failure",
			path:       InstancePath,
			readerErr:  errors.New("disk failed"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "read_error",
			wantCalled: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &fakeReader{err: test.readerErr}
			var logs bytes.Buffer
			handler, err := NewHandler(reader, AllowAll, log.New(&logs, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
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
			if (reader.called > 0) != test.wantCalled {
				t.Fatalf("reader called = %d, want called %t", reader.called, test.wantCalled)
			}
			if test.wantCode == "read_error" && !strings.Contains(logs.String(), "disk failed") {
				t.Fatalf("server log = %q", logs.String())
			}
		})
	}
}
