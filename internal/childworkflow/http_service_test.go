package childworkflow

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
)

type childUnusedReader struct{ readservice.Reader }

func childHTTPFixture(t *testing.T, authenticated bool) (http.Handler, *SubmissionService, *submissionAuthority, string) {
	t.Helper()
	s, resolver, _ := submissionFixture(t)
	key, err := podauth.NewSignedKey(bytes.Repeat([]byte{7}, podauth.MinSignedKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	key.WithClock(s.Now)
	o := resolver.current.Origin
	token, grant, err := key.MintChildWorkflowGrant(podauth.ChildWorkflowGrant{
		Gaggle: o.Gaggle, RunID: o.RunID, StageOccurrence: o.StageOccurrence, AttemptID: o.AttemptID,
		ConfigDigest: o.ConfigDigest, PolicyDigest: o.PolicyDigest,
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resolver.current.Origin.GrantID = grant.ID
	bindSubmissionOrigin(t, s, resolver.current.Origin, o.GrantID)
	options := []httpapi.HandlerOption{httpapi.WithChildWorkflowService(&HTTPService{Submission: s, Grants: key})}
	authorizer := httpapi.AllowAll
	if authenticated {
		auth, err := podauth.NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
		if err != nil {
			t.Fatal(err)
		}
		options = append(options, httpapi.WithAuthenticator(auth.WithChildWorkflowGrants(key)))
		authorizer = httpapi.RequireRoles()
	}
	handler, err := httpapi.NewHandler(childUnusedReader{}, authorizer, log.New(io.Discard, "", 0), options...)
	if err != nil {
		t.Fatal(err)
	}
	return handler, s, resolver, token
}

func childHTTPCall(t *testing.T, handler http.Handler, token, run, operation, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+run+"/child-workflows/"+operation, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		request.Header.Set(httpapi.HeaderIdempotencyKey, key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if strings.Contains(response.Body.String(), token) {
		t.Fatal("response leaked the stage bearer")
	}
	return response
}

func TestChildHTTPServiceUsesRealGrantAndDurableSubmission(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		t.Run(map[bool]string{false: "loopback", true: "authenticated"}[authenticated], func(t *testing.T) {
			handler, s, resolver, token := childHTTPFixture(t, authenticated)
			run := resolver.current.Origin.RunID
			body := apicontract.ChildWorkflowSourceRequest{Source: validProposal}
			response := childHTTPCall(t, handler, token, run, "validate", "", body)
			var advisory apicontract.ChildWorkflowValidationResponse
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &advisory) != nil || !advisory.Valid || !advisory.Advisory {
				t.Fatalf("validate: %d %s", response.Code, response.Body)
			}
			pending, err := s.Queue.Pending(t.Context(), 100)
			if err != nil || len(pending) != 0 {
				t.Fatalf("validation accepted execution: %+v %v", pending, err)
			}
			response = childHTTPCall(t, handler, token, run, "start", "call-1", body)
			var first apicontract.ChildWorkflowResponse
			if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &first) != nil || first.State != "queued" || first.Duplicate || first.ChildID == "" {
				t.Fatalf("start: %d %s", response.Code, response.Body)
			}
			response = childHTTPCall(t, handler, token, run, "start", "call-1", body)
			var duplicate apicontract.ChildWorkflowResponse
			if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &duplicate) != nil || !duplicate.Duplicate || duplicate.ChildID != first.ChildID {
				t.Fatalf("duplicate: %d %s", response.Code, response.Body)
			}
			response = childHTTPCall(t, handler, token, run, "status", "", apicontract.ChildWorkflowStatusRequest{InvocationKey: "call-1"})
			var current apicontract.ChildWorkflowResponse
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &current) != nil || current.ChildID != first.ChildID {
				t.Fatalf("status: %d %s", response.Code, response.Body)
			}
			response = childHTTPCall(t, handler, token, run, "start", "call-2", body)
			if response.Code != http.StatusConflict {
				t.Fatalf("occupied occurrence: %d %s", response.Code, response.Body)
			}
			if err := s.Queue.RevokeChildAuthority(t.Context(), resolver.current.Origin.Binding(s.now().Add(time.Hour))); err != nil {
				t.Fatal(err)
			}
			response = childHTTPCall(t, handler, token, run, "status", "", apicontract.ChildWorkflowStatusRequest{InvocationKey: "call-1"})
			if response.Code != http.StatusForbidden {
				t.Fatalf("revoked bearer retained access: %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestChildHTTPServiceRejectsForgedGrantAndWrongParentBeforeEffects(t *testing.T) {
	handler, s, resolver, token := childHTTPFixture(t, false)
	run := resolver.current.Origin.RunID
	body := apicontract.ChildWorkflowSourceRequest{Source: validProposal}
	for _, presented := range []string{podauth.ChildWorkflowGrantPrefix + "forged.mac", token + "x"} {
		response := childHTTPCall(t, handler, presented, run, "start", "call", body)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("forged: %d %s", response.Code, response.Body)
		}
	}
	response := childHTTPCall(t, handler, token, "other-parent", "start", "call", body)
	if response.Code != http.StatusForbidden {
		t.Fatalf("wrong parent: %d %s", response.Code, response.Body)
	}
	response = childHTTPCall(t, handler, token, run, "validate", "", apicontract.ChildWorkflowSourceRequest{Source: "invalid: source"})
	var invalid apicontract.ChildWorkflowValidationResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &invalid) != nil || invalid.Valid || len(invalid.Diagnostics) == 0 {
		t.Fatalf("validation diagnostic: %d %s", response.Code, response.Body)
	}
	response = childHTTPCall(t, handler, token, run, "start", "call", apicontract.ChildWorkflowSourceRequest{Source: "invalid: source"})
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid start: %d %s", response.Code, response.Body)
	}
	pending, err := s.Queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != 0 {
		t.Fatalf("rejected operation retained a start: %+v %v", pending, err)
	}
}
