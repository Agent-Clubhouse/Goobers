package temporalcodec

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

func serverTLSFiles(t *testing.T) (string, string, *http.Client) {
	t.Helper()
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	pair := fixture.TLS.Certificates[0]
	fixture.Close()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	private, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}
	t.Cleanup(transport.CloseIdleConnections)
	return cert, key, &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func TestServerMandatoryOIDCTLSAndBoundedLifecycle(t *testing.T) {
	cfg := fileCodecConfig(t)
	cert, key, httpClient := serverTLSFiles(t)
	if _, err := NewServer(cfg, "127.0.0.1:0", cert, key, nil); err == nil {
		t.Fatal("accepted absent OIDC")
	}
	_, keys := newTestCodec(t)
	auth, sign := testOIDC(t, keys)
	cfg.API.Auth = &instance.APIAuthConfig{OIDC: &instance.OIDCAuthConfig{Issuer: auth.Issuer, Audience: auth.Audience, Roles: instance.OIDCRoleMapping{View: auth.Roles.View}}}
	for _, pair := range [][2]string{{"", ""}, {cert, ""}, {"missing", key}} {
		if _, err := NewServer(cfg, "127.0.0.1:0", pair[0], pair[1], nil); err == nil {
			t.Fatal("accepted missing TLS")
		}
	}
	server, err := NewServer(cfg, "127.0.0.1:0", cert, key, []string{"https://temporal.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if server.ReadTimeout <= 0 || server.ReadHeaderTimeout <= 0 || server.WriteTimeout <= 0 || server.IdleTimeout <= 0 || server.MaxHeaderBytes > 16<<10 {
		t.Fatal("unbounded HTTP server")
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, server, listener) }()
	for _, tc := range []struct {
		token string
		code  int
	}{{"", 401}, {sign("outsider"), 403}, {sign("readers"), 200}} {
		req, err := http.NewRequest(http.MethodPost, "https://"+listener.Addr().String()+"/decode", strings.NewReader(`{"payloads":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tc.token)
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.code {
			t.Fatalf("status %d want %d", resp.StatusCode, tc.code)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("server did not stop")
	}
	if conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("listener survived shutdown")
	}
}

func TestServerCancellationReachesActiveRequest(t *testing.T) {
	cert, key, httpClient := serverTLSFiles(t)
	cfg := fileCodecConfig(t)
	cfg.API.Auth = &instance.APIAuthConfig{OIDC: &instance.OIDCAuthConfig{Issuer: "https://issuer.example.test", Audience: "codec", Roles: instance.OIDCRoleMapping{View: []string{"readers"}}}}
	server, err := NewServer(cfg, "127.0.0.1:0", cert, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served, requested := make(chan error, 1), make(chan struct{})
	go func() { served <- Serve(ctx, server, listener) }()
	go func() {
		defer close(requested)
		resp, err := httpClient.Get("https://" + listener.Addr().String())
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not enter")
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("active request prevented shutdown")
	}
	<-requested
}
