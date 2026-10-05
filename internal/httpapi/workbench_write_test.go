package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/workbench"
)

type workbenchWriterStub struct {
	calls                        int
	actor                        Principal
	gaggle, source, key, command string
	patch                        workbench.BacklogPatchRequest
	refusal                      error
}

func (s *workbenchWriterStub) record(p Principal, g, b string) error {
	s.calls++
	s.actor, s.gaggle, s.source = p, g, b
	return s.refusal
}
func (s *workbenchWriterStub) Capabilities(_ context.Context, p Principal, g, b string) (workbench.BacklogWriteCapabilities, error) {
	return workbench.BacklogWriteCapabilities{}, s.record(p, g, b)
}
func (s *workbenchWriterStub) Patch(_ context.Context, p Principal, g, b, key string, r workbench.BacklogPatchRequest) (workbench.BacklogEditCommand, error) {
	s.key, s.patch = key, r
	return workbench.BacklogEditCommand{State: "unknown"}, s.record(p, g, b)
}
func (s *workbenchWriterStub) Command(_ context.Context, p Principal, g, b, id string) (workbench.BacklogEditCommand, error) {
	s.command = id
	return workbench.BacklogEditCommand{ID: id, State: "attempting"}, s.record(p, g, b)
}

const nativeWritePath = "http://example.com/api/v1/gaggles/team/workbench/sources/issues/items/42"
const nativeWriteBody = `{"sourceId":"987654","expectedRevision":"revision-one","field":"title","value":"Revised"}`

func nativeWriteHandler(t *testing.T, s *workbenchWriterStub, p *Principal) http.Handler {
	t.Helper()
	h, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: p}), WithWorkbenchWrites(s), WithWorkbenchReads(&workbenchStub{}))
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func nativeWriteRequest(h http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set(HeaderIdempotencyKey, key)
	}
	out := httptest.NewRecorder()
	h.ServeHTTP(out, r)
	return out
}
func TestWorkbenchWriteRoutesBindVerifiedHumanAndItemPath(t *testing.T) {
	s := &workbenchWriterStub{}
	h := nativeWriteHandler(t, s, &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleOperate}})
	out := nativeWriteRequest(h, http.MethodPatch, nativeWritePath, nativeWriteBody, "one")
	if out.Code != 200 || out.Header().Get("Cache-Control") != "no-store" || !strings.Contains(out.Body.String(), `"state":"unknown"`) {
		t.Fatal(out.Code, out.Body)
	}
	if s.actor.Subject != "alice" || s.actor.Issuer != "https://identity.example" || s.gaggle != "team" || s.source != "issues" || s.patch.ID != "42" || s.patch.SourceID != "987654" || s.key != "one" {
		t.Fatal(s)
	}
	base := "http://example.com/api/v1/gaggles/team/workbench/sources/issues"
	for _, path := range []string{base + "/write-capabilities", base + "/commands/workbench-" + strings.Repeat("a", 32)} {
		out = nativeWriteRequest(h, http.MethodGet, path, "", "")
		if out.Code != 200 {
			t.Fatal(out.Code, out.Body)
		}
	}
	// The original read route remains reachable on the same item path.
	if out = nativeWriteRequest(h, http.MethodGet, nativeWritePath, "", ""); out.Code != 200 {
		t.Fatal(out.Code, out.Body)
	}
}
func TestWorkbenchWriteClosedBodyAndTransport(t *testing.T) {
	s := &workbenchWriterStub{}
	h := nativeWriteHandler(t, s, &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleOperate}})
	for _, body := range []string{
		`null`, `[]`, `{}`, nativeWriteBody + `{}`, strings.Replace(nativeWriteBody, `"sourceId":`, `"SourceId":`, 1), strings.Replace(nativeWriteBody, `"sourceId":"987654"`, `"sourceId":"987654","sourceId":"different"`, 1), strings.Replace(nativeWriteBody, `"value":"Revised"`, `"value":null`, 1), strings.Replace(nativeWriteBody, `"value":"Revised"`, `"values":["Revised"]`, 1), strings.Replace(nativeWriteBody, `"value":"Revised"`, `"value":"Revised","values":[]`, 1), strings.Replace(nativeWriteBody, `"value":"Revised"`, `"value":"Revised","id":"different"`, 1), strings.Replace(nativeWriteBody, `"value":"Revised"`, `"value":"Revised","actor":{"subject":"other"}`, 1), strings.Replace(nativeWriteBody, `"value":"Revised"`, `"value":"`+strings.Repeat("a", 4097)+`"`, 1), strings.Repeat(" ", maxWorkbenchPatchBytes) + nativeWriteBody,
	} {
		out := nativeWriteRequest(h, http.MethodPatch, nativeWritePath, body, "one")
		if out.Code != 400 {
			t.Fatalf("closed request accepted, status=%d body=%.200s", out.Code, body)
		}
	}
	for _, key := range []string{"", " padded ", strings.Repeat("a", 201)} {
		if out := nativeWriteRequest(h, http.MethodPatch, nativeWritePath, nativeWriteBody, key); out.Code != 400 {
			t.Fatal(key, out.Code)
		}
	}
	for _, change := range []func(*http.Request){func(r *http.Request) { r.Header.Add(HeaderIdempotencyKey, "two") }, func(r *http.Request) { r.Header.Set("Origin", "https://foreign.example") }, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, func(r *http.Request) { r.URL.RawQuery = "credential=automation" }} {
		r := httptest.NewRequest(http.MethodPatch, nativeWritePath, strings.NewReader(nativeWriteBody))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(HeaderIdempotencyKey, "one")
		change(r)
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		if out.Code != 400 {
			t.Fatal(out.Code, out.Body)
		}
	}
	if s.calls != 0 {
		t.Fatal("malformed transport reached effect service")
	}
}
func TestWorkbenchWriteHumanOnlyAndSafeReceiptErrors(t *testing.T) {
	for _, p := range []*Principal{nil, {Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleView}}, {Issuer: PodPrincipalIssuer, Subject: "worker", Roles: []Role{RoleAdmin}}, {Issuer: "https://identity.example", Subject: "worker", Roles: []Role{RoleAdmin}, GeneratedChild: true}} {
		s := &workbenchWriterStub{}
		h := nativeWriteHandler(t, s, p)
		out := nativeWriteRequest(h, http.MethodPatch, nativeWritePath, nativeWriteBody, "one")
		if out.Code < 400 || s.calls != 0 {
			t.Fatal("nonoperator reached write service", out.Code)
		}
	}
	s := &workbenchWriterStub{refusal: &InterventionError{Status: 410, Code: "workbench_command_expired", Message: "The receipt has expired."}}
	h := nativeWriteHandler(t, s, &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleOperate}})
	base := "http://example.com/api/v1/gaggles/team/workbench/sources/issues/commands/"
	out := nativeWriteRequest(h, http.MethodGet, base+"workbench-"+strings.Repeat("a", 32), "", "")
	if out.Code != 410 || out.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(out.Code, out.Body)
	}
	for _, path := range []string{base + "not-a-command", base + "workbench-" + strings.Repeat("G", 32), base + "workbench-" + strings.Repeat("a", 32) + "?actor=other"} {
		out = nativeWriteRequest(h, http.MethodGet, path, "", "")
		if out.Code != 400 {
			t.Fatal(out.Code, out.Body)
		}
	}
	if s.calls != 1 {
		t.Fatal("invalid receipt lookup reached service")
	}
}
