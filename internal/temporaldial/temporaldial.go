// Package temporaldial is the one place a goobers process builds the options
// it dials a Temporal frontend with (#5289). Every dial site — the daemon's
// shared engine client, the worker host and its orphan sweep, the
// engine-start/-project/-queues commands and the k8s preflight — goes through
// Options, so transport security is configured once rather than five times.
//
// TLS is opt-in. A nil *TLS yields exactly the options every site built before
// this package existed ({HostPort, Namespace}), which is the plaintext dial a
// local dev Temporal expects. There is deliberately no "prefer" mode: when TLS
// is configured and the frontend does not speak it, the dial fails.
package temporaldial

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"go.temporal.io/sdk/client"
)

// TLS is the client transport security for the Temporal frontend connection:
// the instance config's engine.tls block. A non-nil value, even an empty one,
// turns TLS on; empty fields fall back to the Go defaults (system roots, the
// dialed host as the verified name, no client certificate).
type TLS struct {
	// CAFile is a PEM bundle of the CA(s) that sign the frontend's
	// certificate. When set it is the ONLY trust anchor: a private in-cluster
	// CA is pinned rather than added to the system pool, so a certificate
	// from any public CA for the same name is not accepted. Empty uses the
	// system pool.
	CAFile string `json:"caFile,omitempty" yaml:"caFile,omitempty"`
	// CertFile and KeyFile are the PEM client certificate and key presented
	// for mTLS. Both or neither.
	CertFile string `json:"certFile,omitempty" yaml:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty" yaml:"keyFile,omitempty" credentialPath:"file"`
	// ServerName overrides the name verified against the frontend's
	// certificate — needed when the dialed address (a ClusterIP, a port
	// forward) is not a name on the certificate.
	ServerName string `json:"serverName,omitempty" yaml:"serverName,omitempty"`
}

// Validate checks the static shape of the block; file contents are checked at
// dial time by Config.
func (t *TLS) Validate() error {
	if t == nil {
		return nil
	}
	if (t.CertFile == "") != (t.KeyFile == "") {
		return errors.New("certFile and keyFile must be set together")
	}
	return nil
}

// Transport names the transport a dial with t negotiates, for operator-facing
// reporting: "plaintext", "tls" or "mtls".
func (t *TLS) Transport() string {
	switch {
	case t == nil:
		return "plaintext"
	case t.CertFile != "":
		return "mtls"
	default:
		return "tls"
	}
}

// Config builds the *tls.Config t describes. A nil t returns nil.
func (t *TLS) Config() (*tls.Config, error) {
	if t == nil {
		return nil, nil
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("temporal tls: %w", err)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: t.ServerName}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("temporal tls: read caFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("temporal tls: caFile %s holds no PEM certificate", t.CAFile)
		}
		cfg.RootCAs = pool
	}
	if t.CertFile != "" {
		pair, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("temporal tls: load client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// Options builds the client options for one Temporal dial. With t nil the
// result is exactly client.Options{HostPort, Namespace}.
func Options(hostPort, namespace string, t *TLS) (client.Options, error) {
	opts := client.Options{HostPort: hostPort, Namespace: namespace}
	cfg, err := t.Config()
	if err != nil {
		return client.Options{}, err
	}
	if cfg != nil {
		opts.ConnectionOptions.TLS = cfg
	}
	return opts, nil
}

// Dial connects to the frontend with the options Options builds. A failed
// TLS dial names the transport, so a frontend that only speaks plaintext is
// reported as a TLS failure rather than a bare connection error.
func Dial(ctx context.Context, hostPort, namespace string, t *TLS) (client.Client, error) {
	opts, err := Options(hostPort, namespace, t)
	if err != nil {
		return nil, err
	}
	c, err := client.DialContext(ctx, opts)
	if err != nil && t != nil {
		return nil, fmt.Errorf("temporal %s dial to %s: %w", t.Transport(), hostPort, err)
	}
	return c, err
}
