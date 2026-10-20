package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func testHumanAuth(t *testing.T) *humanAuth {
	t.Helper()
	a, err := newHumanAuth([]string{"alice", "bob"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func bootstrapHuman(t *testing.T, a *humanAuth, handler http.Handler) []*http.Cookie {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"token": a.bootstrap})
	r := httptest.NewRequest("POST", "http://127.0.0.1:8097/auth/exchange", bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://127.0.0.1:8097")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("bootstrap status=%d", w.Code)
	}
	if strings.Contains(w.Body.String(), a.session) {
		t.Fatal("bootstrap reflected session cookie in response body")
	}
	var binding struct {
		CSRF string `json:"csrf"`
	}
	if json.Unmarshal(w.Body.Bytes(), &binding) != nil || binding.CSRF != a.csrf {
		t.Fatal("bootstrap omitted origin-scoped header binding")
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Value == a.csrf {
			t.Fatal("header binding leaked into port-shared cookies")
		}
	}
	return w.Result().Cookies()
}

func TestHumanAuthenticationProtectsEveryControlAndReadRoute(t *testing.T) {
	a := testHumanAuth(t)
	dispatched := 0
	h := authenticatedPrototypeHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatched++
		if r.Context().Value(actorKey{}) != "alice" {
			t.Error("persona cookie supplied authority")
		}
		w.WriteHeader(204)
	}), "127.0.0.1", "8097", a)
	for _, path := range []string{
		"/sessions", "/sessions/s/commands", "/identity/select", "/config/reload",
		"/status", "/surfaces", "/sessions/s/events", "/sessions/s/changes", "/operations/state?sessionId=s",
		"/operations/actions/a/apply?sessionId=s", "/connections/c/revoke", "/future-control",
	} {
		for _, method := range []string{"GET", "POST"} {
			r := httptest.NewRequest(method, "http://127.0.0.1:8097"+api+path, strings.NewReader(`{"type":"child.review","decision":"approve"}`))
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(&http.Cookie{Name: "goobers-local-actor", Value: "bob"})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 401 {
				t.Fatalf("unauthenticated %s %s status=%d", method, path, w.Code)
			}
		}
	}
	if dispatched != 0 {
		t.Fatal("unauthenticated request reached control router")
	}
	cookies := bootstrapHuman(t, a, h)
	for _, csrf := range []string{"", "wrong", a.csrf} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8097"+api+"/sessions", strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(humanCSRFHeader, csrf)
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 403
		if csrf == a.csrf {
			want = 204
		}
		if w.Code != want {
			t.Fatalf("CSRF binding status=%d, want %d", w.Code, want)
		}
	}
	if dispatched != 1 {
		t.Fatalf("unexpected authorized dispatch count %d", dispatched)
	}
}

func TestHumanAuthenticationRejectsForgedSessionAndPersona(t *testing.T) {
	a := testHumanAuth(t)
	h := authenticatedPrototypeHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("forged credential reached controls")
		w.WriteHeader(204)
	}), "127.0.0.1", "8097", a)
	cookies := bootstrapHuman(t, a, h)
	for _, forged := range []string{humanCookie, "goobers-local-actor"} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8097"+api+"/sessions", strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(humanCSRFHeader, a.csrf)
		for _, cookie := range cookies {
			if cookie.Name != forged {
				r.AddCookie(cookie)
			}
		}
		r.AddCookie(&http.Cookie{Name: forged, Value: "bob"})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("forged %s status=%d", forged, w.Code)
		}
	}
}

func TestPortSharedCookiesCannotReadStreamOrControlWithoutOriginBinding(t *testing.T) {
	a := testHumanAuth(t)
	h := authenticatedPrototypeHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("cookie-only request reached authenticated routes")
	}), "127.0.0.1", "8097", a)
	cookies := bootstrapHuman(t, a, h)
	for _, path := range []string{"/status", "/sessions/s/events", "/sessions/s/commands", "/operations/actions/a/apply"} {
		for _, method := range []string{"GET", "POST"} {
			r := httptest.NewRequest(method, "http://127.0.0.1:8097"+api+path, strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			for _, cookie := range cookies {
				r.AddCookie(cookie)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("cookie-only %s %s status=%d", method, path, w.Code)
			}
		}
	}
}

func TestHumanBootstrapIsOneUseAndPersonaRequiresAuthenticatedHuman(t *testing.T) {
	a := testHumanAuth(t)
	bootstrap := a.bootstrap
	h := authenticatedPrototypeHandler(http.NotFoundHandler(), "127.0.0.1", "8097", a)
	cookies := bootstrapHuman(t, a, h)
	for _, cookie := range cookies {
		if cookie.SameSite != http.SameSiteStrictMode || cookie.Name == humanCookie && !cookie.HttpOnly {
			t.Fatal("human cookie is not isolated from script/cross-site requests")
		}
	}
	data, _ := json.Marshal(map[string]string{"token": bootstrap})
	r := httptest.NewRequest("POST", "http://127.0.0.1:8097/auth/exchange", bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://127.0.0.1:8097")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || a.bootstrap != "" {
		t.Fatal("bootstrap token remained reusable")
	}
	r = httptest.NewRequest("POST", "http://127.0.0.1:8097"+api+"/identity/select", nil)
	if err := a.selectActor(w, r, "bob"); err == nil {
		t.Fatal("unauthenticated persona selection accepted")
	}
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	r.Header.Set(humanCSRFHeader, a.csrf)
	w = httptest.NewRecorder()
	if err := a.selectActor(w, r, "bob"); err != nil {
		t.Fatal(err)
	}
	r.Header.Del("Cookie")
	for _, cookie := range cookies {
		if cookie.Name != "goobers-local-actor" {
			r.AddCookie(cookie)
		}
	}
	for _, cookie := range w.Result().Cookies() {
		r.AddCookie(cookie)
	}
	if actor, err := a.authorize(r); err != nil || actor != "bob" {
		t.Fatal("authorized persona selection lost")
	}
	other := testHumanAuth(t)
	bootstrapHuman(t, other, authenticatedPrototypeHandler(http.NotFoundHandler(), "127.0.0.1", "8097", other))
	if _, err := other.authorize(r); err == nil {
		t.Fatal("another server accepted a prior human session")
	}
}

func TestHumanAuthStillRejectsHostOriginAndCrossSiteRequests(t *testing.T) {
	a := testHumanAuth(t)
	h := authenticatedPrototypeHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), "127.0.0.1", "8097", a)
	cookies := bootstrapHuman(t, a, h)
	for _, scenario := range []string{"host", "origin", "cross-site"} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8097"+api+"/sessions", strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(humanCSRFHeader, a.csrf)
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		switch scenario {
		case "host":
			r.Host = "attacker.invalid:8097"
		case "origin":
			r.Header.Set("Origin", "http://attacker.invalid")
		case "cross-site":
			r.Header.Set("Sec-Fetch-Site", "cross-site")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("%s request status=%d", scenario, w.Code)
		}
	}
}

func TestHumanBootstrapLinkIsPrivateAndRemovedOnExchange(t *testing.T) {
	a := testHumanAuth(t)
	path, err := a.writeBootstrapLink(t.TempDir(), "127.0.0.1:8097")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("bootstrap link is not owner-private")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "http://127.0.0.1:8097/auth#"+a.bootstrap+"\n" {
		t.Fatal("bootstrap link was not stored in the fragment")
	}
	h := authenticatedPrototypeHandler(http.NotFoundHandler(), "127.0.0.1", "8097", a)
	bootstrapHuman(t, a, h)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("used bootstrap link was retained")
	}
}
