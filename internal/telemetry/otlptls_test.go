package telemetry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	telemetrytest "github.com/goobers/goobers/test/testsupport/telemetry"
)

// otlpTestCertificate is a self-signed leaf used as its own trust anchor —
// the same technique internal/httpapi/auth_test.go's selfSignedCertFiles
// uses: AppendCertsFromPEM accepts a self-signed cert as a root exactly like
// a real CA, so the same PEM file serves as both a server's certificate and
// a client's caFile.
type otlpTestCertificate struct {
	certFile, keyFile string
	certificate       *x509.Certificate
}

func generateOTLPTestCertificate(t *testing.T, dnsNames []string, ips []net.IP) otlpTestCertificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "goobers-otlp-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
		t.Fatal(err)
	}
	return otlpTestCertificate{certFile: certFile, keyFile: keyFile, certificate: certificate}
}

// startTLSOTLPCollector serves the trace collector over TLS using
// serverCert, optionally requiring and verifying a client certificate signed
// by clientCACert (mTLS). Returns the listener address and a channel
// receiving one export per accepted, authenticated connection.
func startTLSOTLPCollector(t *testing.T, serverCert otlpTestCertificate, clientCACert *otlpTestCertificate) (addr string, requests chan *collectortrace.ExportTraceServiceRequest) {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(serverCert.certFile, serverCert.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	serverTLSConfig := &tls.Config{Certificates: []tls.Certificate{pair}}
	if clientCACert != nil {
		pool := x509.NewCertPool()
		pool.AddCert(clientCACert.certificate)
		serverTLSConfig.ClientCAs = pool
		serverTLSConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLSConfig)))
	requests = make(chan *collectortrace.ExportTraceServiceRequest, 1)
	collectortrace.RegisterTraceServiceServer(server, &recordingOTLPCollector{
		requests: requests,
		headers:  make(chan metadata.MD, 1),
	})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	return listener.Addr().String(), requests
}

// waitForExport waits up to `timeout` for one export. want=false uses a
// short timeout — like TestOTLPExporterSecureModeOverridesInsecureEnvironment,
// it only needs to observe that nothing arrives promptly; the underlying
// gRPC attempt is free to keep retrying in the background until
// t.Cleanup's Shutdown tears it down, so a longer wait would only slow the
// test down without adding confidence.
func waitForExport(t *testing.T, requests chan *collectortrace.ExportTraceServiceRequest, want bool) {
	t.Helper()
	timeout := 250 * time.Millisecond
	if want {
		timeout = 5 * time.Second
	}
	select {
	case <-requests:
		if !want {
			t.Fatal("collector received an export it should have refused (TLS trust/identity was not enforced)")
		}
	case <-time.After(timeout):
		if want {
			t.Fatal("collector received no export within the deadline")
		}
	}
}

// TestNewOTLPExporterTrustsConfiguredCAFile is #3804's core acceptance: a
// collector presenting a certificate the system trust store does not know
// about is unreachable with no tls block (mirrors
// TestOTLPExporterSecureModeOverridesInsecureEnvironment's negative shape),
// and reachable once OTLPCAFile names that certificate.
func TestNewOTLPExporterTrustsConfiguredCAFile(t *testing.T) {
	serverCert := generateOTLPTestCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	addr, requests := startTLSOTLPCollector(t, serverCert, nil)

	t.Run("untrusted without caFile", func(t *testing.T) {
		client, err := New(context.Background(), Config{
			ServiceName:  "otlp-tls-test",
			Exporter:     ExporterOTLP,
			OTLPEndpoint: addr,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
		emitTestSpan(t, client)
		waitForExport(t, requests, false)
	})

	t.Run("trusted with caFile", func(t *testing.T) {
		client, err := New(context.Background(), Config{
			ServiceName:  "otlp-tls-test",
			Exporter:     ExporterOTLP,
			OTLPEndpoint: addr,
			OTLPCAFile:   serverCert.certFile,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
		emitTestSpan(t, client)
		waitForExport(t, requests, true)
	})
}

// TestNewOTLPExporterAppliesServerNameOverride proves OTLPServerName is
// actually wired into the exporter's TLS config, not merely accepted: the
// certificate carries a DNS SAN but no IP SAN, so dialing its IP address
// verifies only when ServerName overrides SNI/verification to the SAN name.
func TestNewOTLPExporterAppliesServerNameOverride(t *testing.T) {
	serverCert := generateOTLPTestCertificate(t, []string{"otlp-collector.internal.test"}, nil)
	addr, requests := startTLSOTLPCollector(t, serverCert, nil)

	t.Run("hostname mismatch without serverName", func(t *testing.T) {
		client, err := New(context.Background(), Config{
			ServiceName:  "otlp-servername-test",
			Exporter:     ExporterOTLP,
			OTLPEndpoint: addr,
			OTLPCAFile:   serverCert.certFile,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
		emitTestSpan(t, client)
		waitForExport(t, requests, false)
	})

	t.Run("verifies with serverName override", func(t *testing.T) {
		client, err := New(context.Background(), Config{
			ServiceName:    "otlp-servername-test",
			Exporter:       ExporterOTLP,
			OTLPEndpoint:   addr,
			OTLPCAFile:     serverCert.certFile,
			OTLPServerName: "otlp-collector.internal.test",
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
		emitTestSpan(t, client)
		waitForExport(t, requests, true)
	})
}

// TestNewOTLPExporterPresentsClientCertificate proves OTLPCertFile/
// OTLPKeyFile are wired into the exporter's TLS config for mTLS: the
// collector requires and verifies a client certificate, so the export only
// reaches it once a client certificate is configured.
func TestNewOTLPExporterPresentsClientCertificate(t *testing.T) {
	serverCert := generateOTLPTestCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	clientCert := generateOTLPTestCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	addr, requests := startTLSOTLPCollector(t, serverCert, &clientCert)

	t.Run("refused without a client certificate", func(t *testing.T) {
		client, err := New(context.Background(), Config{
			ServiceName:  "otlp-mtls-test",
			Exporter:     ExporterOTLP,
			OTLPEndpoint: addr,
			OTLPCAFile:   serverCert.certFile,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
		emitTestSpan(t, client)
		waitForExport(t, requests, false)
	})

	t.Run("accepted with configured client certificate", func(t *testing.T) {
		client, err := New(context.Background(), Config{
			ServiceName:  "otlp-mtls-test",
			Exporter:     ExporterOTLP,
			OTLPEndpoint: addr,
			OTLPCAFile:   serverCert.certFile,
			OTLPCertFile: clientCert.certFile,
			OTLPKeyFile:  clientCert.keyFile,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
		emitTestSpan(t, client)
		waitForExport(t, requests, true)
	})
}

// TestNewDegradesToLocalOnlyWhenOTLPTLSMaterialInvalid is #3804's decided
// degrade behavior: a bad CA/cert/key path must not turn into a daemon boot
// failure (the L-28 shape in a new location). New still returns a usable
// *Client — the local SpanExporter is fully wired — with an error wrapping
// ErrOTLPUnavailable that names the bad path, for the caller to log loudly
// (cmd/goobers/daemon.go's telemetry_otlp_unavailable instance-journal
// event) instead of failing setup.
func TestNewDegradesToLocalOnlyWhenOTLPTLSMaterialInvalid(t *testing.T) {
	validCert := generateOTLPTestCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})

	cases := []struct {
		name    string
		cfg     func(t *testing.T, local *telemetrytest.MemoryExporter) Config
		wantErr string
	}{
		{
			name: "unreadable ca file",
			cfg: func(t *testing.T, local *telemetrytest.MemoryExporter) Config {
				return Config{
					ServiceName:  "otlp-degrade-test",
					SpanExporter: local,
					Batch:        true,
					Exporter:     ExporterOTLP,
					OTLPEndpoint: "127.0.0.1:1",
					OTLPCAFile:   filepath.Join(t.TempDir(), "missing-ca.crt"),
				}
			},
			wantErr: "missing-ca.crt",
		},
		{
			name: "malformed ca pem",
			cfg: func(t *testing.T, local *telemetrytest.MemoryExporter) Config {
				dir := t.TempDir()
				bad := filepath.Join(dir, "bad-ca.crt")
				if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
					t.Fatal(err)
				}
				return Config{
					ServiceName:  "otlp-degrade-test",
					SpanExporter: local,
					Batch:        true,
					Exporter:     ExporterOTLP,
					OTLPEndpoint: "127.0.0.1:1",
					OTLPCAFile:   bad,
				}
			},
			wantErr: "no certificates found",
		},
		{
			name: "unreadable client key",
			cfg: func(t *testing.T, local *telemetrytest.MemoryExporter) Config {
				return Config{
					ServiceName:  "otlp-degrade-test",
					SpanExporter: local,
					Batch:        true,
					Exporter:     ExporterOTLP,
					OTLPEndpoint: "127.0.0.1:1",
					OTLPCertFile: validCert.certFile,
					OTLPKeyFile:  filepath.Join(t.TempDir(), "missing.key"),
				}
			},
			wantErr: "load client certificate",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := telemetrytest.NewMemoryExporter()
			client, err := New(context.Background(), tc.cfg(t, local))
			if client == nil {
				t.Fatal("New() returned a nil client on an OTLP TLS degrade — local-only telemetry must still work")
			}
			if err == nil || !errors.Is(err, ErrOTLPUnavailable) {
				t.Fatalf("New() error = %v, want it to wrap ErrOTLPUnavailable", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("New() error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
			t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

			// The client stays functional locally: a span reaches the local
			// exporter even though the collector export never got wired.
			_, span, err := client.StartRun(context.Background(), RunAttributes{
				Gaggle: "acme-web", WorkflowID: "wf", RunID: "0af7651916cd43dd8448eb211c80319c",
			})
			if err != nil {
				t.Fatal(err)
			}
			span.End()
			if err := client.FlushLocal(context.Background()); err != nil {
				t.Fatalf("FlushLocal() on a degraded client = %v, want nil", err)
			}
			if got := len(local.Spans()); got != 1 {
				t.Fatalf("local exporter captured %d spans on a degraded client, want 1", got)
			}
		})
	}
}

// emitTestSpan starts and ends one span without forcing a flush —
// SimpleSpanProcessor (Config.Batch unset) exports synchronously in End(),
// and a failing export's own retry/backoff must not become this test's
// timeline (mirrors TestOTLPExporterSecureModeOverridesInsecureEnvironment).
func emitTestSpan(t *testing.T, client *Client) {
	t.Helper()
	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "acme-web", WorkflowID: "wf", RunID: "0af7651916cd43dd8448eb211c80319c",
	})
	if err != nil {
		t.Fatal(err)
	}
	span.End()
}
