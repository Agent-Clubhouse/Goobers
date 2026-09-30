package journalclient

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/daemonclient"
)

// The default journal-plane client reaches a daemon behind a private CA when
// the daemon CA is configured and is refused at certificate verification
// when it is not (#5286).
func TestHTTPDefaultClientTrustsTheConfiguredDaemonCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"runId":"run-1","events":[]}`))
	}))
	t.Cleanup(server.Close)
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))

	events := func(t *testing.T) error {
		t.Helper()
		backend, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "tok", RunID: "run-1"})
		if err != nil {
			t.Fatalf("NewHTTP: %v", err)
		}
		_, err = backend.EventsContext(context.Background())
		return err
	}

	t.Setenv(daemonclient.CAEnv, caPEM)
	if err := events(t); err != nil {
		t.Fatalf("configured daemon CA: %v", err)
	}

	t.Setenv(daemonclient.CAEnv, "")
	err := events(t)
	var refused *tls.CertificateVerificationError
	if !errors.As(err, &refused) {
		t.Fatalf("unset daemon CA: err = %v, want a certificate verification refusal", err)
	}
}
