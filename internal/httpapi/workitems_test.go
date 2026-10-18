package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/readservice"
)

func TestWorkItemRoutesListAndDrillIntoActions(t *testing.T) {
	reader := &fakeReader{
		workItems: readservice.WorkItemPage{Items: []readservice.WorkItemSummary{{
			Provider: "github", Repository: "acme/app", Kind: "pr", ExternalID: "42", ActionCount: 2,
		}}},
		workItem: readservice.WorkItemDetail{Actions: []readservice.WorkItemAction{{
			RunID: "run-1", Operation: "comment",
		}}},
	}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, WorkItemsPath+"?kind=pr&provider=github&gaggle=core&limit=25", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %q", list.Code, list.Body.String())
	}
	if reader.workItemsReq.Kind != "pr" || reader.workItemsReq.Provider != "github" ||
		reader.workItemsReq.Gaggle != "core" || reader.workItemsReq.Limit != 25 {
		t.Fatalf("list request = %#v", reader.workItemsReq)
	}

	detail := httptest.NewRecorder()
	handler.ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/v1/work-items/github/pr/42?repository=acme%2Fapp", nil))
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body = %q", detail.Code, detail.Body.String())
	}
	var decoded readservice.WorkItemDetail
	if err := json.NewDecoder(detail.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Provider != "github" || decoded.Repository != "acme/app" ||
		decoded.Kind != "pr" || decoded.ExternalID != "42" ||
		len(decoded.Actions) != 1 || decoded.Actions[0].Operation != "comment" {
		t.Fatalf("detail = %#v", decoded)
	}
}

// TestWorkItemDetailNamesTheInvalidParameter pins #6799: a detail request
// missing its repository (or carrying a bad kind or a stray parameter) must
// say which parameter is wrong rather than the generic "telemetry query is
// invalid", and must not reach the reader.
func TestWorkItemDetailNamesTheInvalidParameter(t *testing.T) {
	for _, tc := range []struct {
		name, target, want string
	}{
		{"missing repository", "/api/v1/work-items/ado/pr/42", "repository query parameter is required"},
		{"blank repository", "/api/v1/work-items/ado/pr/42?repository=%2F", "repository query parameter is required"},
		{"invalid kind", "/api/v1/work-items/ado/pull/42?repository=org%2Fproject%2Frepo", "kind must be pr or issue"},
		{"unknown parameter", "/api/v1/work-items/ado/pr/42?repository=org%2Fproject%2Frepo&repo=x", `unknown query parameter "repo"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeReader{}
			handler, err := NewHandler(reader, AllowAll, discardLogger())
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.target, nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
			}
			var envelope ErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != "invalid_query" || !strings.Contains(envelope.Error.Message, tc.want) {
				t.Fatalf("error = %+v, want invalid_query naming %q", envelope.Error, tc.want)
			}
			if reader.workItem.ExternalID != "" {
				t.Fatalf("reader was called for an invalid request: %+v", reader.workItem)
			}
		})
	}
}

func TestWorkItemListRejectsInvalidKind(t *testing.T) {
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, WorkItemsPath+"?kind=pull", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
}
