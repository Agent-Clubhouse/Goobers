package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestContainedPodCannotReachOperationalPlanes(t *testing.T) {
	for _, parent := range []bool{false, true} {
		p := Principal{Issuer: PodPrincipalIssuer, Subject: "run:run-1", GeneratedChild: !parent, WorkflowParent: parent, Roles: []Role{RoleAdmin}}
		for _, route := range podRouteTable() {
			r := httptest.NewRequest(route.method, route.path, nil)
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, p))
			allowed := route.name == "claim list" || route.name == "credential resolve" || route.name == "journal emit" || route.name == "surrender" || route.name == "blob read" || route.name == "blob write"
			if got := RequireRoles().Authorize(r) == nil; got != allowed {
				t.Errorf("parent=%v route=%s allowed=%v want=%v", parent, route.name, got, allowed)
			}
		}
		for _, path := range []string{"/api/v1/runs/foreign/journal/emit", "/api/v1/runs/foreign/stages/s/attempts/1/surrender", apicontract.ClaimVerifyPath, "/api/v1/runs/run-1/operator-messages"} {
			r := httptest.NewRequest(http.MethodPost, path, nil)
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, p))
			if RequireRoles().Authorize(r) == nil {
				t.Errorf("contained parent=%v admitted %s", parent, path)
			}
		}
	}
}

func TestContainedPodJournalRefusesControlBeforeWriter(t *testing.T) {
	for _, parent := range []bool{false, true} {
		p := &Principal{Issuer: PodPrincipalIssuer, Subject: "run:run-1", GeneratedChild: !parent, WorkflowParent: parent}
		service := &fakeJournalService{}
		h := writePlaneHandler(t, &fakeAuthenticator{principal: p}, RequireRoles(), WithJournalService(service))
		observation := livejournal.Op{Kind: livejournal.OpAppend, Event: &journal.Event{Type: journal.EventStageHeartbeat, Stage: "build", Attempt: 1}}
		cases := []struct {
			name    string
			input   livejournal.EmitRequest
			allowed bool
		}{
			{"heartbeat", livejournal.EmitRequest{Ops: []livejournal.Op{observation}}, true},
			{"control", livejournal.EmitRequest{Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Event: &journal.Event{Type: journal.EventRunFinished, Status: "success"}}}}, false},
			{"open", livejournal.EmitRequest{Open: &livejournal.OpenHeader{}, Ops: []livejournal.Op{observation}}, false},
			{"annotation", livejournal.EmitRequest{Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Event: &journal.Event{Type: journal.EventRunnerAnnotation, Stage: "build", Runner: map[string]any{"kind": "child.workflow.continued"}}}}}, false},
			{"hidden control", livejournal.EmitRequest{Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Event: &journal.Event{Type: journal.EventStageHeartbeat, Stage: "build", Status: "success", Target: "@complete"}}}}, false},
			{"namespace", livejournal.EmitRequest{RunID: "foreign", Ops: []livejournal.Op{observation}}, false},
		}
		for _, tc := range cases {
			before := len(service.requests)
			data, _ := json.Marshal(tc.input)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/v1/runs/run-1/journal/emit", string(data)))
			if got := rec.Code == http.StatusOK; got != tc.allowed {
				t.Errorf("parent=%v %s status=%d body=%s", parent, tc.name, rec.Code, rec.Body)
			}
			if !tc.allowed && len(service.requests) != before {
				t.Fatal("control reached journal service")
			}
		}
	}
}

func TestContainedClaimListingCannotWidenToNamespace(t *testing.T) {
	p := Principal{Issuer: PodPrincipalIssuer, Subject: "run:run-1", GeneratedChild: true}
	r := httptest.NewRequest(http.MethodPost, apicontract.ClaimListPath, nil)
	r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, p))
	good := ClaimListRequest{RunID: "run-1", Scope: ClaimListScopeRun, Execution: true, IncludeHistory: true}
	if err := validateContainedClaimList(r, good); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ClaimListRequest){func(in *ClaimListRequest) { in.Scope = ClaimListScopeNamespace }, func(in *ClaimListRequest) { in.RunID = "foreign" }, func(in *ClaimListRequest) { in.Execution = false }, func(in *ClaimListRequest) { in.Provider = "github" }} {
		in := good
		change(&in)
		if validateContainedClaimList(r, in) == nil {
			t.Fatal("claim observation widened", in)
		}
	}
}
