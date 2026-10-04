package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/sessioning"
)

type repairOperationFixture struct{ operationFixture }

func (f *repairOperationFixture) InspectSelectedPR(context.Context, string, string, sessioning.PRRepairInspectRequest) (sessioning.PRRepairInspection, error) {
	f.calls++
	return sessioning.PRRepairInspection{}, nil
}
func (f *repairOperationFixture) ReadSelectedPRFile(context.Context, string, string, sessioning.PRRepairReadRequest) (sessioning.PRRepairFile, error) {
	f.calls++
	return sessioning.PRRepairFile{}, nil
}
func (f *repairOperationFixture) RepairSelectedPR(context.Context, string, string, sessioning.PRRepairRequest) (sessioning.PRRepairCommandView, error) {
	f.calls++
	return sessioning.PRRepairCommandView{}, nil
}
func (f *repairOperationFixture) PRRepairReceipt(context.Context, string, string, sessioning.PRRepairReceiptRequest) (sessioning.PRRepairCommandView, error) {
	f.calls++
	return sessioning.PRRepairCommandView{}, nil
}
func TestSessionPRRepairHTTPGrantBodyAndInstallation(t *testing.T) {
	service := &repairOperationFixture{}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(DenyAllAuthenticator{}), WithSessionOperations(service))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, run, body string
		status          int
	}{
		{"inspect_selected_pr", "run-one", `{}`, 200},
		{"read_selected_pr_file", "run-one", `{"path":"file.txt"}`, 200},
		{"repair_selected_pr", "run-one", `{"requestId":"one","expectedHeadSha":"` + strings.Repeat("a", 40) + `","rationale":"Fix selected PR","changes":[{"path":"file.txt","content":"fixed"}]}`, 200},
		{"get_pr_repair_receipt", "run-one", `{"commandId":"repair-` + strings.Repeat("a", 32) + `"}`, 200},
		{"inspect_selected_pr", "run-two", `{}`, 403},
		{"inspect_selected_pr", "run-one", `{"repository":"foreign"}`, 400},
		{"repair_selected_pr", "run-one", `{"actor":"admin"}`, 400},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+tc.run+"/session-operations/"+tc.name, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+sessioning.OperationTokenPrefix+strings.Repeat("a", 64))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatal(tc.name, w.Code, w.Body.String())
		}
	}
	if service.calls != 4 {
		t.Fatal(service.calls)
	}
	for _, id := range []apicontract.RouteID{apicontract.RouteSessionPRRepairInspect, apicontract.RouteSessionPRRepairRead, apicontract.RouteSessionPRRepair, apicontract.RouteSessionPRRepairReceipt} {
		if ok, _, _ := routeAvailability(id, handlerConfig{sessionOperations: &operationFixture{}}); ok {
			t.Fatal("generic reader advertised repairs")
		}
		if ok, _, _ := routeAvailability(id, handlerConfig{sessionOperations: service}); !ok {
			t.Fatal("installed repair unavailable")
		}
	}
}
