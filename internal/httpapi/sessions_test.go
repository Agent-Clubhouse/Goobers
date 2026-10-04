package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/sessioning"
)

type sessionStub struct {
	calls                              int
	principal                          Principal
	gaggle, session, key, text, cursor string
	after                              uint64
	limit                              int
	refusal                            error
}

func (s *sessionStub) called(p Principal, gaggle, session string) error {
	s.calls++
	s.principal = p
	s.gaggle = gaggle
	s.session = session
	return s.refusal
}
func (s *sessionStub) Create(_ context.Context, p Principal, g string, r sessioning.CreateRequest) (sessioning.Acceptance, error) {
	s.key = r.RequestID
	s.text = r.Title
	return sessioning.Acceptance{}, s.called(p, g, "")
}
func (s *sessionStub) Get(_ context.Context, p Principal, g, id string) (sessioning.Session, error) {
	return sessioning.Session{}, s.called(p, g, id)
}
func (s *sessionStub) List(_ context.Context, p Principal, g, cursor string, limit int) (sessioning.SessionPage, error) {
	s.cursor = cursor
	s.limit = limit
	return sessioning.SessionPage{Items: []sessioning.Session{}}, s.called(p, g, "")
}
func (s *sessionStub) Messages(_ context.Context, p Principal, g, id string, after uint64, limit int) (sessioning.MessagePage, error) {
	s.after = after
	s.limit = limit
	return sessioning.MessagePage{Items: []sessioning.Message{}}, s.called(p, g, id)
}
func (s *sessionStub) SubmitMessage(_ context.Context, p Principal, g, id string, r sessioning.MessageRequest) (sessioning.Acceptance, error) {
	s.key = r.RequestID
	s.text = r.Text
	return sessioning.Acceptance{}, s.called(p, g, id)
}
func (s *sessionStub) Close(_ context.Context, p Principal, g, id string, r sessioning.CloseRequest) (sessioning.Acceptance, error) {
	s.key = r.RequestID
	s.text = r.Reason
	return sessioning.Acceptance{}, s.called(p, g, id)
}

func sessionTestHandler(t *testing.T, service *sessionStub, auth *fakeAuthenticator) http.Handler {
	t.Helper()
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(auth), WithInteractiveSessions(service))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}
func sessionRequest(handler http.Handler, method, path, body, key, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://example.com/api/v1/gaggles/team/sessions"+path, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	return out
}
func TestSharedSessionRoutesBindVerifiedActorAndRejectAuthorityOverrides(t *testing.T) {
	s := &sessionStub{}
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "https://issuer.example", Subject: "alice", Roles: []Role{RoleOperate}}}
	h := sessionTestHandler(t, s, auth)
	for _, test := range []struct {
		name, path, body, key, origin string
		want                          int
	}{
		{"create", "", `{"title":"Plan work","goober":"planner"}`, "c1", "", 202},
		{"message", "/session-one/messages", `{"text":"Add details"}`, "m1", "", 202},
		{"close", "/session-one/close", `{"reason":"Done"}`, "x1", "", 202},
		{"null-close", "/session-one/close", `null`, "null", "", 400},
		{"missing-key", "/session-one/messages", `{"text":"Add details"}`, "", "", 400},
		{"forged-actor", "/session-one/messages", `{"text":"Add details","actor":{"subject":"other"}}`, "m2", "", 400},
		{"forged-request-id", "/session-one/messages", `{"text":"Add details","requestId":"other"}`, "m2", "", 400},
		{"forged-pins", "", `{"title":"Plan work","goober":"planner","configGeneration":"other"}`, "c2", "", 400},
		{"query", "/session-one/messages?gaggle=other", `{"text":"Add details"}`, "m3", "", 400},
		{"cross-origin", "/session-one/close", `{"reason":"Done"}`, "x2", "https://other.example", 400},
		{"empty-message", "/session-one/messages", `{"text":"  "}`, "m4", "", 400},
		{"large-utf8", "/session-one/messages", `{"text":"` + strings.Repeat("語", 22000) + `"}`, "m5", "", 400},
		{"two-objects", "/session-one/messages", `{"text":"Add"}{"text":"Other"}`, "m6", "", 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := s.calls
			out := sessionRequest(h, http.MethodPost, test.path, test.body, test.key, test.origin)
			if out.Code != test.want || out.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(out.Code, out.Body)
			}
			if test.want != 202 && s.calls != before {
				t.Fatal("invalid input reached service")
			}
			if test.want == 202 && (s.principal.Subject != "alice" || s.principal.Issuer != "https://issuer.example" || s.gaggle != "team" || s.key != test.key) {
				t.Fatal(s)
			}
		})
	}
}

func TestSharedSessionReadsBoundPaginationAndCurrentPermission(t *testing.T) {
	s := &sessionStub{}
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "https://issuer.example", Subject: "alice", Roles: []Role{RoleView}}}
	h := sessionTestHandler(t, s, auth)
	for _, test := range []struct {
		path string
		want int
	}{
		{"?cursor=session-before&limit=20", 200}, {"/session-one", 200}, {"/session-one/messages?after=12&limit=10", 200},
		{"?limit=201", 400}, {"?limit=0", 400}, {"?limit=1&limit=2", 400}, {"?cursor=", 400}, {"?actor=other", 400}, {"/session-one?cursor=other", 400}, {"/session-one/messages?after=-1", 400}, {"/session-one/messages?after=9007199254740992", 400}, {"/session-one/messages?after=%zz", 400},
	} {
		before := s.calls
		out := sessionRequest(h, http.MethodGet, test.path, "", "", "")
		if out.Code != test.want {
			t.Fatal(test.path, out.Code, out.Body)
		}
		if test.want != 200 && s.calls != before {
			t.Fatal("invalid cursor reached service")
		}
	}
	if s.after != 12 || s.limit != 10 || s.cursor != "session-before" {
		t.Fatal(s)
	}
	s.refusal = &InterventionError{Status: 403, Code: "interactive_access_denied", Message: "Session access denied."}
	out := sessionRequest(h, http.MethodGet, "/session-one/messages", "", "", "")
	if out.Code != 403 || strings.Contains(out.Body.String(), "items") {
		t.Fatal(out.Code, out.Body)
	}
	before := s.calls
	if out = sessionRequest(h, http.MethodPost, "/session-one/messages", `{"text":"No"}`, "key", ""); out.Code != 403 || s.calls != before {
		t.Fatal(out.Code, s.calls)
	}
	auth.principal = &Principal{Issuer: PodPrincipalIssuer, Subject: "pod", Roles: []Role{RoleAdmin}}
	if out = sessionRequest(h, http.MethodGet, "/session-one/messages", "", "", ""); out.Code != 403 || s.calls != before {
		t.Fatal(out.Code, s.calls)
	}
}
