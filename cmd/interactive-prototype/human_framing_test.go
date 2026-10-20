package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestGatekeeperHumanPortalDisallowsFraming(t *testing.T) {
	a := testHumanAuth(t)
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<!doctype html><title>Portal</title>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("void 0;")},
	})))
	mux.HandleFunc(api+"/status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc(api+"/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
	})
	h := authenticatedPrototypeHandler(mux, "127.0.0.1", "8097", a)
	cookies := bootstrapHuman(t, a, h)
	for _, path := range []string{"/", "/assets/app.js", api + "/status", api + "/events", "/auth"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://127.0.0.1:8097"+path, nil)
			r.Header.Set("Sec-Fetch-Site", "same-site")
			r.Header.Set("Sec-Fetch-Dest", "iframe")
			r.Header.Set(humanCSRFHeader, a.csrf)
			for _, c := range cookies {
				r.AddCookie(c)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 && w.Code != 204 {
				t.Fatalf("unexpected fixture status %d", w.Code)
			}
			if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || w.Header().Get("X-Frame-Options") != "DENY" {
				t.Fatalf("frameable response: CSP=%q X-Frame-Options=%q", w.Header().Get("Content-Security-Policy"), w.Header().Get("X-Frame-Options"))
			}
		})
	}
	t.Run("rejected request", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://foreign.invalid:8097/", nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("unexpected fixture status %d", w.Code)
		}
		if w.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" || w.Header().Get("X-Frame-Options") != "DENY" {
			t.Fatal("rejected request lacks framing protection")
		}
	})
}
