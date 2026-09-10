package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, WorkItemsPath+"?kind=pr&provider=github&limit=25", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %q", list.Code, list.Body.String())
	}
	if reader.workItemsReq.Kind != "pr" || reader.workItemsReq.Provider != "github" || reader.workItemsReq.Limit != 25 {
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
