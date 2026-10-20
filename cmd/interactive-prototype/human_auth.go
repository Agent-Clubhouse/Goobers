package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

const humanCookie = "goobers-human-session"
const humanCSRFCookie = "goobers-human-csrf"
const humanCSRFHeader = "X-Goobers-CSRF"

var errHumanUnauthenticated = errors.New("open the one-time authentication link in the server's private human-auth.url file before using interactive controls")
var errHumanCSRF = errors.New("human control request requires the session CSRF header")

// humanAuth is private to this server process. Secrets are never put in driver
// requests, environment, workspace files, JSON response bodies or the persona cookie.
// The one-use bootstrap fragment lives in a private file in denied host state.
type humanAuth struct {
	mu        sync.Mutex
	bootstrap string
	session   string
	csrf      string
	actor     string
	actors    []string
	personas  map[string]string
	ready     bool
	linkPath  string
}

func humanSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func newHumanAuth(actors []string) (*humanAuth, error) {
	if len(actors) == 0 {
		return nil, fmt.Errorf("human authentication requires a configured demo persona")
	}
	secrets := make([]string, 3)
	for i := range secrets {
		var err error
		secrets[i], err = humanSecret()
		if err != nil {
			return nil, fmt.Errorf("create human authentication secret: %w", err)
		}
	}
	a := &humanAuth{bootstrap: secrets[0], session: secrets[1], csrf: secrets[2], actor: actors[0], actors: slices.Clone(actors), personas: map[string]string{}}
	for _, actor := range actors {
		token, err := humanSecret()
		if err != nil {
			return nil, fmt.Errorf("create demo persona binding: %w", err)
		}
		a.personas[token] = actor
	}
	return a, nil
}

func sameSecret(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (a *humanAuth) writeBootstrapLink(stateDir, address string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	file, err := os.CreateTemp(stateDir, ".human-auth-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, writeErr := io.WriteString(file, "http://"+address+"/auth#"+a.bootstrap+"\n")
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return "", err
	}
	path := filepath.Join(stateDir, "human-auth.url")
	if err := os.Rename(file.Name(), path); err != nil {
		return "", err
	}
	a.linkPath = path
	return path, nil
}

func (a *humanAuth) removeBootstrapLink() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.linkPath != "" {
		_ = os.Remove(a.linkPath)
	}
}

func (a *humanAuth) authorize(r *http.Request) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cookie, err := r.Cookie(humanCookie)
	if err != nil || !a.ready || !sameSecret(cookie.Value, a.session) {
		return "", errHumanUnauthenticated
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameSecret(r.Header.Get(humanCSRFHeader), a.csrf) {
		return "", errHumanCSRF
	}
	if persona, err := r.Cookie("goobers-local-actor"); err == nil {
		actor, ok := a.personas[persona.Value]
		if !ok {
			return "", errHumanUnauthenticated
		}
		return actor, nil
	}
	return a.actor, nil
}

// selectActor is available only within the controlled-local prototype. The
// authenticated human selects a configured demo persona; cookies cannot forge it.
func (a *humanAuth) selectActor(w http.ResponseWriter, r *http.Request, actor string) error {
	if _, err := a.authorize(r); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !slices.Contains(a.actors, actor) {
		return fmt.Errorf("unknown controlled local actor")
	}
	a.personaCookie(w, actor)
	return nil
}

func (a *humanAuth) personaCookie(w http.ResponseWriter, actor string) {
	for token, bound := range a.personas {
		if bound == actor {
			http.SetCookie(w, &http.Cookie{Name: "goobers-local-actor", Value: token, Path: api, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			return
		}
	}
}

func (a *humanAuth) serveBootstrap(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/auth" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Authenticate interactive controls</title><p id="status">Authenticating this browser…</p><script src="/auth/bootstrap.js" defer></script>`))
		return
	}
	if r.URL.Path == "/auth/bootstrap.js" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(`const token = location.hash.slice(1);
history.replaceState(null, "", "/auth");
fetch("/auth/exchange", {method:"POST", credentials:"same-origin", headers:{"Content-Type":"application/json"}, body:JSON.stringify({token})}).then(r => {
  if (!r.ok) throw new Error("Authentication failed or this one-time URL was already used. Restart the server to obtain a new URL.");
  location.replace("/#/interactive");
}).catch(e => { document.getElementById("status").textContent = e.message; });`))
		return
	}
	if r.URL.Path != "/auth/exchange" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Origin") != "http://"+r.Host || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, http.StatusForbidden, fmt.Errorf("bootstrap requires a same-origin JSON request"))
		return
	}
	var input struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		fail(w, http.StatusBadRequest, fmt.Errorf("invalid bootstrap request"))
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ready || !sameSecret(input.Token, a.bootstrap) {
		fail(w, http.StatusUnauthorized, fmt.Errorf("invalid or already used bootstrap token"))
		return
	}
	a.ready, a.bootstrap = true, ""
	if a.linkPath != "" {
		_ = os.Remove(a.linkPath)
	}
	http.SetCookie(w, &http.Cookie{Name: humanCookie, Value: a.session, Path: api, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.SetCookie(w, &http.Cookie{Name: humanCSRFCookie, Value: a.csrf, Path: "/", SameSite: http.SameSiteStrictMode})
	a.personaCookie(w, a.actor)
	writeJSON(w, map[string]string{"status": "authenticated"})
}

// All control routes share this boundary, including future routes registered
// beneath /api/. The human secret never becomes model/tool authority.
func authenticatedPrototypeHandler(next http.Handler, address, port string, human *humanAuth) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		host, requestPort, err := net.SplitHostPort(r.Host)
		if err != nil || requestPort != port || (host != address && host != "localhost") {
			http.Error(w, "unexpected host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/auth" || strings.HasPrefix(r.URL.Path, "/auth/") {
			human.serveBootstrap(w, r)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		owner, err := human.authorize(r)
		if err != nil {
			status := http.StatusUnauthorized
			if errors.Is(err, errHumanCSRF) {
				status = http.StatusForbidden
			}
			fail(w, status, err)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "application/json is required", http.StatusUnsupportedMediaType)
			return
		}
		ctx := context.WithValue(r.Context(), actorKey{}, owner)
		ctx = context.WithValue(ctx, methodKey{}, r.Method)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
