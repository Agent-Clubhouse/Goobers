package daemonclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// privateCADaemon is a TLS daemon whose certificate chains to no system
// root, standing in for a daemon behind a privately issued certificate. It
// returns the server and its CA as PEM.
func privateCADaemon(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return server, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
}

// unrelatedCA is a freshly minted self-signed CA the test daemon's
// certificate does not chain to.
func unrelatedCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func get(client *http.Client, url string) error {
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errors.New("unexpected status " + response.Status)
	}
	return nil
}

// Unset must leave the client exactly what every call site built before the
// constructor existed: no transport, no TLS config, only the timeout.
func TestNewHTTPUnsetIsTheBareTimeoutClient(t *testing.T) {
	for _, value := range []string{"", "   \n"} {
		t.Setenv(CAEnv, value)
		got := NewHTTP(7 * time.Second)
		want := &http.Client{Timeout: 7 * time.Second}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s=%q: NewHTTP = %#v, want %#v", CAEnv, value, got, want)
		}
		if got.Transport != nil {
			t.Fatalf("%s=%q: transport = %T, want nil (http.DefaultTransport)", CAEnv, value, got.Transport)
		}
	}
}

func TestNewHTTPUnsetRefusesAPrivateCADaemon(t *testing.T) {
	server, _ := privateCADaemon(t)
	t.Setenv(CAEnv, "")
	err := get(NewHTTP(5*time.Second), server.URL)
	var refused *tls.CertificateVerificationError
	if !errors.As(err, &refused) {
		t.Fatalf("unset bundle reached a private-CA daemon: err = %v, want a certificate verification refusal", err)
	}
}

func TestNewHTTPTrustsThePEMContentsBundle(t *testing.T) {
	server, caPEM := privateCADaemon(t)
	t.Setenv(CAEnv, caPEM)
	client := NewHTTP(5 * time.Second)
	if client.Timeout != 5*time.Second {
		t.Fatalf("timeout = %v, want 5s", client.Timeout)
	}
	if err := get(client, server.URL); err != nil {
		t.Fatalf("configured bundle did not reach the daemon: %v", err)
	}
}

func TestNewHTTPTrustsTheBundlePath(t *testing.T) {
	server, caPEM := privateCADaemon(t)
	path := filepath.Join(t.TempDir(), "daemon-ca.pem")
	if err := os.WriteFile(path, []byte(caPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(CAEnv, path)
	if err := get(NewHTTP(5*time.Second), server.URL); err != nil {
		t.Fatalf("bundle path did not reach the daemon: %v", err)
	}
}

func TestNewHTTPWithAnUnrelatedBundleStillRefuses(t *testing.T) {
	server, _ := privateCADaemon(t)
	t.Setenv(CAEnv, unrelatedCA(t))
	err := get(NewHTTP(5*time.Second), server.URL)
	var refused *tls.CertificateVerificationError
	if !errors.As(err, &refused) {
		t.Fatalf("an unrelated bundle reached the daemon: err = %v, want a certificate verification refusal", err)
	}
}

// A configured bundle that cannot be used fails closed with the
// configuration error rather than falling back to the system store.
func TestNewHTTPFailsClosedOnAnUnusableBundle(t *testing.T) {
	server, _ := privateCADaemon(t)
	for name, tc := range map[string]struct{ value, want string }{
		"missing file": {value: filepath.Join(t.TempDir(), "absent.pem"), want: "read " + CAEnv},
		"no cert":      {value: "-----BEGIN CERTIFICATE-----\nnot base64\n-----END CERTIFICATE-----\n", want: "holds no PEM certificate"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(CAEnv, tc.value)
			err := get(NewHTTP(5*time.Second), server.URL)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}
}
