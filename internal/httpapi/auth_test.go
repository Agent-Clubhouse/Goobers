package httpapi

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/readservice"
)

func requestWithPrincipal(method string, principal *Principal) *http.Request {
	request := httptest.NewRequest(method, HealthPath, nil)
	if principal != nil {
		request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, *principal))
	}
	return request
}

func TestRequireRolesEnforcesFloorPerMethod(t *testing.T) {
	authorizer := RequireRoles()
	tests := []struct {
		name      string
		method    string
		principal *Principal
		allowed   bool
	}{
		{name: "anonymous read denied", method: http.MethodGet, principal: nil, allowed: false},
		{name: "authenticated without roles denied", method: http.MethodGet, principal: &Principal{Subject: "s"}, allowed: false},
		{name: "view can read", method: http.MethodGet, principal: &Principal{Subject: "s", Roles: []Role{RoleView}}, allowed: true},
		{name: "view can head", method: http.MethodHead, principal: &Principal{Subject: "s", Roles: []Role{RoleView}}, allowed: true},
		{name: "view cannot mutate", method: http.MethodPost, principal: &Principal{Subject: "s", Roles: []Role{RoleView}}, allowed: false},
		{name: "operate can read", method: http.MethodGet, principal: &Principal{Subject: "s", Roles: []Role{RoleOperate}}, allowed: true},
		{name: "operate can mutate", method: http.MethodDelete, principal: &Principal{Subject: "s", Roles: []Role{RoleOperate}}, allowed: true},
		{name: "admin can mutate", method: http.MethodPost, principal: &Principal{Subject: "s", Roles: []Role{RoleAdmin}}, allowed: true},
		{name: "unknown role grants nothing", method: http.MethodGet, principal: &Principal{Subject: "s", Roles: []Role{Role("root")}}, allowed: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := authorizer.Authorize(requestWithPrincipal(test.method, test.principal))
			if test.allowed && err != nil {
				t.Fatalf("Authorize() = %v, want nil", err)
			}
			if !test.allowed && err == nil {
				t.Fatal("Authorize() = nil, want error")
			}
		})
	}
}

func TestRoleAuthorizerAnswers401And403(t *testing.T) {
	reader := &fakeReader{health: readservice.Health{Ready: true}}
	authenticator := &fakeAuthenticator{principal: &Principal{Subject: "viewer"}}
	handler, err := NewHandler(reader, RequireRoles(), discardLogger(), WithAuthenticator(authenticator))
	if err != nil {
		t.Fatal(err)
	}

	// Authenticated but unmapped: deny by default with 403.
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, HealthPath, nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var envelope ErrorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "forbidden" {
		t.Fatalf("error = %+v", envelope.Error)
	}

	// Unauthenticated: 401 before authorization.
	authenticator.err = errors.New("bad token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, HealthPath, nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}

	// View role passes the read floor.
	authenticator.err = nil
	authenticator.principal = &Principal{Subject: "viewer", Roles: []Role{RoleView}}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, HealthPath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}

// TestServerRefusesNonLoopbackWithoutTLSAndAuthenticator proves the #640
// fail-closed startup rule: construction fails before any socket exists —
// Start (the only place a listener opens) is never reachable.
func TestServerRefusesNonLoopbackWithoutTLSAndAuthenticator(t *testing.T) {
	certFile, keyFile, _ := selfSignedCertFiles(t)
	nullHandler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	authedHandler, err := NewHandler(
		&fakeReader{},
		RequireRoles(),
		discardLogger(),
		WithAuthenticator(&fakeAuthenticator{principal: &Principal{Subject: "s", Roles: []Role{RoleView}}}),
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := NewServer("0.0.0.0:0", nullHandler, discardLogger()); err == nil ||
		!strings.Contains(err.Error(), "without TLS") {
		t.Fatalf("plain non-loopback error = %v, want TLS refusal", err)
	}
	if _, err := NewServer("0.0.0.0:0", nullHandler, discardLogger(), WithTLS(certFile, keyFile)); err == nil ||
		!strings.Contains(err.Error(), "without an authenticator") {
		t.Fatalf("unauthenticated non-loopback error = %v, want authenticator refusal", err)
	}
	if _, err := NewServer("0.0.0.0:0", authedHandler, discardLogger()); err == nil ||
		!strings.Contains(err.Error(), "without TLS") {
		t.Fatalf("authenticated plaintext non-loopback error = %v, want TLS refusal", err)
	}
	if _, err := NewServer("0.0.0.0:0", authedHandler, discardLogger(), WithTLS(certFile, keyFile)); err != nil {
		t.Fatalf("hardened non-loopback NewServer = %v, want nil", err)
	}
	// A null authenticator passed explicitly is still null — fail closed.
	explicitNull, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithAuthenticator(NullAuthenticator{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer("0.0.0.0:0", explicitNull, discardLogger(), WithTLS(certFile, keyFile)); err == nil {
		t.Fatal("expected explicit NullAuthenticator to be refused off-loopback")
	}
}

func TestServerServesTLSOnConfiguredCertificate(t *testing.T) {
	certFile, keyFile, certificate := selfSignedCertFiles(t)
	handler, err := NewHandler(&fakeReader{health: readservice.Health{Ready: true}}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer("127.0.0.1:0", handler, discardLogger(), WithTLS(certFile, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := server.Scheme(); got != "https" {
		t.Fatalf("Scheme() = %q, want https", got)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	response, err := client.Get("https://" + server.Address() + HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}

	// The listener speaks only TLS: a plaintext request must not succeed.
	plain := &http.Client{Timeout: 2 * time.Second}
	plainResponse, err := plain.Get("http://" + server.Address() + HealthPath)
	if err == nil {
		_ = plainResponse.Body.Close()
		if plainResponse.StatusCode == http.StatusOK {
			t.Fatal("plaintext request succeeded against a TLS listener")
		}
	}

	if _, err := NewServer("127.0.0.1:0", handler, discardLogger(), WithTLS(filepath.Join(t.TempDir(), "missing.crt"), keyFile)); err == nil {
		t.Fatal("expected missing certificate file to fail construction")
	}
	if _, err := NewServer("127.0.0.1:0", handler, discardLogger(), WithTLS("", "")); err == nil {
		t.Fatal("expected empty cert/key paths to fail construction")
	}
}

// TestDefaultLoopbackPostureUnchanged pins the zero-config transport #640
// leaves alone: plain HTTP on loopback, no Authorization header required,
// no TLS in play.
func TestDefaultLoopbackPostureUnchanged(t *testing.T) {
	handler, err := NewHandler(&fakeReader{health: readservice.Health{Ready: true}}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer("127.0.0.1:0", handler, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if got := server.Scheme(); got != "http" {
		t.Fatalf("Scheme() = %q, want http", got)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	response, err := http.Get("http://" + server.Address() + HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var health readservice.Health
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if !health.Ready {
		t.Fatalf("health = %+v", health)
	}
}

// TestEventStreamServesUnderAuthenticator proves the SSE endpoint keeps
// working when a real Authenticator + role authorizer gate the API, and that
// it rejects unauthenticated subscribers like every other route.
func TestEventStreamServesUnderAuthenticator(t *testing.T) {
	_, _, stream := newEventStreamFixture(t)
	authenticator := &tokenAuthenticator{
		token:     "stream-token",
		principal: Principal{Subject: "viewer", Roles: []Role{RoleView}},
	}
	handler, err := NewHandler(
		&fakeReader{health: readservice.Health{Ready: true}},
		RequireRoles(),
		discardLogger(),
		WithEventStream(stream),
		WithAuthenticator(authenticator),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	request, err := http.NewRequest(http.MethodGet, server.URL+EventsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated stream status = %d, want 401", response.StatusCode)
	}

	request, err = http.NewRequest(http.MethodGet, server.URL+EventsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer stream-token")
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated stream status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	reader := bufio.NewReader(response.Body)
	deadline := time.After(5 * time.Second)
	lines := make(chan string, 1)
	go func() {
		line, err := reader.ReadString('\n')
		if err == nil {
			lines <- line
		}
	}()
	select {
	case line := <-lines:
		if !strings.HasPrefix(line, "event:") && !strings.HasPrefix(line, "id:") && !strings.HasPrefix(line, ":") {
			t.Fatalf("unexpected first stream line %q", line)
		}
	case <-deadline:
		t.Fatal("no stream payload within deadline")
	}
}

type tokenAuthenticator struct {
	token     string
	principal Principal
}

func (a *tokenAuthenticator) Authenticate(request *http.Request) (*Principal, error) {
	if request.Header.Get("Authorization") != "Bearer "+a.token {
		return nil, errors.New("missing or wrong bearer token")
	}
	principal := a.principal
	return &principal, nil
}

// selfSignedCertFiles writes a loopback server certificate/key pair into a
// temp dir and returns their paths plus the parsed certificate for client
// trust pools.
func selfSignedCertFiles(t *testing.T) (certFile, keyFile string, certificate *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "goobers-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "api.crt")
	keyFile = filepath.Join(dir, "api.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, certificate
}
