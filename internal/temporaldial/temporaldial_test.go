package temporaldial

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// TestOptionsUnsetIsTodaysDial pins the rollback posture: with no TLS block
// the options are exactly what every dial site built before #5289.
func TestOptionsUnsetIsTodaysDial(t *testing.T) {
	got, err := Options("temporal:7233", "goobers", nil)
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	want := client.Options{HostPort: "temporal:7233", Namespace: "goobers"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Options(nil TLS) = %#v, want %#v", got, want)
	}
	if got := (*TLS)(nil).Transport(); got != "plaintext" {
		t.Fatalf("nil Transport = %q, want plaintext", got)
	}
}

func TestOptionsTLSConfig(t *testing.T) {
	pki := newTestPKI(t)

	t.Run("empty block enables TLS with system roots", func(t *testing.T) {
		opts, err := Options("h:1", "ns", &TLS{})
		if err != nil {
			t.Fatalf("Options: %v", err)
		}
		cfg := opts.ConnectionOptions.TLS
		if cfg == nil || cfg.RootCAs != nil || len(cfg.Certificates) != 0 || cfg.MinVersion != tls.VersionTLS12 {
			t.Fatalf("TLS config = %#v", cfg)
		}
		if opts.HostPort != "h:1" || opts.Namespace != "ns" {
			t.Fatalf("host/namespace lost: %#v", opts)
		}
	})
	t.Run("caFile pins the CA and serverName is carried", func(t *testing.T) {
		opts, err := Options("h:1", "ns", &TLS{CAFile: pki.caFile, ServerName: "frontend.test"})
		if err != nil {
			t.Fatalf("Options: %v", err)
		}
		cfg := opts.ConnectionOptions.TLS
		if cfg.RootCAs == nil || cfg.ServerName != "frontend.test" || len(cfg.Certificates) != 0 {
			t.Fatalf("TLS config = %#v", cfg)
		}
		if (&TLS{CAFile: pki.caFile}).Transport() != "tls" {
			t.Fatal("CA-only transport should report tls")
		}
	})
	t.Run("cert and key present a client certificate", func(t *testing.T) {
		mtls := &TLS{CAFile: pki.caFile, CertFile: pki.clientCert, KeyFile: pki.clientKey}
		opts, err := Options("h:1", "ns", mtls)
		if err != nil {
			t.Fatalf("Options: %v", err)
		}
		if n := len(opts.ConnectionOptions.TLS.Certificates); n != 1 {
			t.Fatalf("client certificates = %d, want 1", n)
		}
		if mtls.Transport() != "mtls" {
			t.Fatalf("Transport = %q, want mtls", mtls.Transport())
		}
	})

	bad := filepath.Join(t.TempDir(), "notpem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		tls  *TLS
		want string
	}{
		"cert without key": {&TLS{CertFile: pki.clientCert}, "certFile and keyFile must be set together"},
		"key without cert": {&TLS{KeyFile: pki.clientKey}, "certFile and keyFile must be set together"},
		"missing caFile":   {&TLS{CAFile: filepath.Join(t.TempDir(), "absent")}, "read caFile"},
		"caFile not PEM":   {&TLS{CAFile: bad}, "holds no PEM certificate"},
		"bad key pair":     {&TLS{CertFile: pki.clientCert, KeyFile: bad}, "load client certificate"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Options("h:1", "ns", tc.tls); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Options error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// TestDialAgainstLocalFrontends drives the real SDK dial (health of the
// connection is proven by the eager GetSystemInfo the SDK issues) against
// in-process gRPC frontends: a plaintext one, a TLS one, and one requiring a
// client certificate. It is the acceptance matrix of #5289 — configured TLS
// connects only to TLS, unset stays plaintext — without a Temporal server.
func TestDialAgainstLocalFrontends(t *testing.T) {
	pki := newTestPKI(t)
	plain := startFrontend(t, nil)
	tlsOnly := startFrontend(t, pki.serverTLS(false))
	mtlsOnly := startFrontend(t, pki.serverTLS(true))

	caOnly := &TLS{CAFile: pki.caFile, ServerName: "frontend.test"}
	mtls := &TLS{CAFile: pki.caFile, CertFile: pki.clientCert, KeyFile: pki.clientKey, ServerName: "frontend.test"}

	cases := []struct {
		name    string
		addr    string
		tls     *TLS
		wantErr string
	}{
		{"unset dials a plaintext frontend", plain, nil, ""},
		{"unset does not negotiate TLS", tlsOnly, nil, "failed reaching server"},
		{"tls dials a TLS frontend", tlsOnly, caOnly, ""},
		{"tls refuses a plaintext frontend", plain, caOnly, "temporal tls dial to"},
		{"tls without the pinned CA refuses the frontend", tlsOnly, &TLS{ServerName: "frontend.test"}, "temporal tls dial to"},
		{"mtls dials an mTLS frontend", mtlsOnly, mtls, ""},
		{"tls without a client certificate is refused by an mTLS frontend", mtlsOnly, caOnly, "temporal tls dial to"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := Dial(ctx, tc.addr, "default", tc.tls)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Dial: %v", err)
				}
				c.Close()
				return
			}
			if err == nil {
				c.Close()
				t.Fatalf("Dial succeeded, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Dial error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestNoDialSiteBypassesTheConstructor keeps the constructor the only way a
// production file builds Temporal client options: a new site that writes its
// own client.Options literal or calls client.Dial would silently dial
// plaintext whatever engine.tls says.
func TestNoDialSiteBypassesTheConstructor(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "temporaldial" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := string(src)
			if !strings.Contains(text, `"go.temporal.io/sdk/client"
 "go.temporal.io/sdk/converter"`) {
				return nil
			}
			if strings.Contains(text, "client.Options{") || strings.Contains(text, "client.Dial(") {
				offenders = append(offenders, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("these files build Temporal client options without temporaldial.Options: %v", offenders)
	}
}

type testFrontend struct {
	workflowservice.UnimplementedWorkflowServiceServer
}

func (testFrontend) GetSystemInfo(context.Context, *workflowservice.GetSystemInfoRequest) (*workflowservice.GetSystemInfoResponse, error) {
	return &workflowservice.GetSystemInfoResponse{}, nil
}

// startFrontend serves the one RPC the SDK's eager dial issues, over TLS when
// cfg is non-nil and plaintext otherwise, and returns its address.
func startFrontend(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var opts []grpc.ServerOption
	if cfg != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(cfg)))
	}
	srv := grpc.NewServer(opts...)
	workflowservice.RegisterWorkflowServiceServer(srv, testFrontend{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

type testPKI struct {
	caFile, clientCert, clientKey string
	ca                            *x509.CertPool
	server                        tls.Certificate
}

func (p testPKI) serverTLS(requireClientCert bool) *tls.Config {
	cfg := &tls.Config{Certificates: []tls.Certificate{p.server}, MinVersion: tls.VersionTLS12}
	if requireClientCert {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = p.ca
	}
	return cfg
}

// newTestPKI mints a private CA, a server certificate for frontend.test and a
// client certificate, writing the client-side PEM files to a temp dir.
func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()
	caKey := newKey(t)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "goobers test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	leaf := func(serial int64, usage x509.ExtKeyUsage, dns []string) ([]byte, []byte) {
		key := newKey(t)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: "goobers test leaf"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
			DNSNames:     dns,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("create leaf: %v", err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatalf("marshal key: %v", err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	}
	serverCertPEM, serverKeyPEM := leaf(2, x509.ExtKeyUsageServerAuth, []string{"frontend.test"})
	server, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("server key pair: %v", err)
	}
	clientCertPEM, clientKeyPEM := leaf(3, x509.ExtKeyUsageClientAuth, nil)

	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	return testPKI{
		caFile:     write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		clientCert: write("client.pem", clientCertPEM),
		clientKey:  write("client-key.pem", clientKeyPEM),
		ca:         pool,
		server:     server,
	}
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func TestOptionsPreservesExplicitConverterWithTLS(t *testing.T) {
	dc := converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), converter.NewZlibCodec(converter.ZlibCodecOptions{AlwaysEncode: true}))
	opts, err := Options("h:1", "ns", &TLS{}, dc)
	if err != nil {
		t.Fatal(err)
	}
	if opts.DataConverter != dc || opts.ConnectionOptions.TLS == nil {
		t.Fatal("converter or TLS dropped")
	}
}
