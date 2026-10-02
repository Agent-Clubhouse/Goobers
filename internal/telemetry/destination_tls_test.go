package telemetry

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNamedTLSRootsAndRotationAreDestinationLocal(t *testing.T) {
	certA := generateOTLPTestCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	certB := generateOTLPTestCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	endpointA, requestsA := startTLSOTLPCollector(t, certA, nil)
	endpointB, requestsB := startTLSOTLPCollector(t, certB, nil)
	rotatingPath := filepath.Join(t.TempDir(), "rotating-ca.pem")
	copyCA := func(path string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(rotatingPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	copyCA(certA.certFile)
	first := NamedDestination{Name: "rotating", Config: Config{Exporter: ExporterOTLP, OTLPEndpoint: endpointA, OTLPCAFile: rotatingPath}}
	second := NamedDestination{Name: "steady", Config: Config{Exporter: ExporterOTLP, OTLPEndpoint: endpointB, OTLPCAFile: certB.certFile}}
	run := func() {
		t.Helper()
		c, err := New(t.Context(), Config{Destinations: []NamedDestination{first, second}})
		if err != nil {
			t.Fatal(err)
		}
		emitNamedRun(t, c)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err = c.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		closeNamedClient(t, c)
	}
	run()
	receiveNamed(t, requestsA)
	receiveNamed(t, requestsB)
	rotated := generateOTLPTestCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	endpointRotated, requestsRotated := startTLSOTLPCollector(t, rotated, nil)
	copyCA(rotated.certFile)
	first.Config.OTLPEndpoint = endpointRotated
	run()
	receiveNamed(t, requestsRotated)
	receiveNamed(t, requestsB)
}
