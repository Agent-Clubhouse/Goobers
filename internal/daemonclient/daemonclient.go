// Package daemonclient builds the HTTP client every pod-side daemon-API
// caller uses when its owner supplies none (#5286).
//
// Unset, it returns exactly the client those callers built before: a bare
// http.Client with a timeout on the default transport and the image's system
// trust store. Set, GOOBERS_DAEMON_CA adds a PEM bundle to the system pool so
// a stage pod can verify a daemon certificate issued by a private CA. That
// matters most on Windows, where Go's crypto/x509 ignores SSL_CERT_FILE and
// reads roots from the system store only.
package daemonclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// CAEnv names the variable carrying the daemon-API CA bundle: either PEM
// contents (simplest to stamp into a Windows pod) or a path to a PEM file.
const CAEnv = "GOOBERS_DAEMON_CA"

// NewHTTP returns the default daemon-API client bounded by timeout.
//
// With GOOBERS_DAEMON_CA unset or blank the result is &http.Client{Timeout:
// timeout}, unchanged from before this constructor existed: no new dial path.
// With it set, the client trusts the system pool plus the bundle. A bundle
// that cannot be read or holds no certificate fails closed: every request
// through the client returns the configuration error instead of silently
// falling back to the system store the operator asked to extend.
func NewHTTP(timeout time.Duration) *http.Client {
	value := strings.TrimSpace(os.Getenv(CAEnv))
	if value == "" {
		return &http.Client{Timeout: timeout}
	}
	transport, err := caTransport(value)
	if err != nil {
		return &http.Client{Timeout: timeout, Transport: refusingTransport{err: err}}
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// caTransport clones the default transport and pins its roots to the system
// pool plus the configured bundle.
func caTransport(value string) (*http.Transport, error) {
	pemBytes, err := bundleBytes(value)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("daemonclient: %s holds no PEM certificate", CAEnv)
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("daemonclient: default transport is not an *http.Transport")
	}
	transport := base.Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return transport, nil
}

// bundleBytes treats a value that opens a PEM block as the bundle itself and
// anything else as a path to one.
func bundleBytes(value string) ([]byte, error) {
	if strings.HasPrefix(value, "-----BEGIN") {
		return []byte(value), nil
	}
	data, err := os.ReadFile(value)
	if err != nil {
		return nil, fmt.Errorf("daemonclient: read %s bundle: %w", CAEnv, err)
	}
	return data, nil
}

// refusingTransport fails every request with the bundle's configuration
// error.
type refusingTransport struct{ err error }

func (t refusingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		_ = request.Body.Close()
	}
	return nil, t.err
}
