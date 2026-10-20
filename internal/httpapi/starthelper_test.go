package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/apicontract/childworkflowwire"
)

type fakeHelperService struct {
	fakeChildWorkflowService
	helper apicontract.HelperRequest
}

func (s *fakeHelperService) StartHelper(_ context.Context, grant, run, key string, helper apicontract.HelperRequest) (apicontract.ChildWorkflowResponse, error) {
	s.record("helper", grant, run, key, nil)
	s.helper = helper
	return apicontract.ChildWorkflowResponse{State: "queued", InvocationKey: key}, s.err
}

func TestStartHelperStrictBodyAndAuthority(t *testing.T) {
	body := `{"helper":{"task":"Inspect\nchanges","goober":"coder","harness":"codex","model":"configured-model","base":"parent"}}`
	service := &fakeHelperService{}
	handler := writePlaneHandler(t, nil, AllowAll, WithChildWorkflowService(service))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, childRequest("start", body))
	if response.Code != http.StatusAccepted || service.calls != 1 || service.operation != "helper" ||
		service.grant != childTestGrant || service.run != "parent-1" || service.key != "inspect-1" ||
		service.helper.Task != "Inspect\nchanges" || service.helper.Model != "configured-model" {
		t.Fatalf("status=%d service=%+v body=%s", response.Code, service, response.Body)
	}
	for _, invalid := range []string{
		`{"helper":null}`, `{"helper":{}}`, `{"helper":{"task":"x"}}`,
		`{"helper":{"task":"x","goober":"coder","Task":"other"}}`,
		`{"helper":{"task":"x","goober":"coder","goober":"other"}}`,
		`{"helper":{"task":"x","goober":"coder","actor":"parent"}}`,
		`{"helper":{"task":"x","goober":"coder","model":null}}`,
		`{"helper":{"task":"x","goober":"coder","base":""}}`,
		`{"helper":{"task":"x","goober":"coder"},"source":"x"}`,
		`{"source":"x","helper":{"task":"x","goober":"coder"}}`,
		`{"helper":{"task":"x","goober":"coder"}}{}`,
		`{"helper":{"task":"` + strings.Repeat("x", childworkflowwire.MaxHelperTaskBytes+1) + `","goober":"coder"}}`,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, childRequest("start", invalid))
		if response.Code != http.StatusBadRequest || service.calls != 1 {
			t.Fatalf("invalid length=%d status=%d calls=%d", len(invalid), response.Code, service.calls)
		}
	}
	response = httptest.NewRecorder()
	request := childRequest("start", body)
	request.Header.Del("Authorization")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || service.calls != 1 {
		t.Fatalf("helper bypassed grant: status=%d calls=%d", response.Code, service.calls)
	}
	response = httptest.NewRecorder()
	writePlaneHandler(t, nil, AllowAll, WithChildWorkflowService(&fakeChildWorkflowService{})).ServeHTTP(response, childRequest("start", body))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing compiler: %d", response.Code)
	}
}
