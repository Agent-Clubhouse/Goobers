package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

const egressHelperEnv = "REVIEWEREVAL_EGRESS_HELPER"

// TestEgressProbeHelperProcess stands in for the reviewereval binary inside a
// sandbox: it runs the egress probe on the hosts after its subcommand. Run
// normally, it does nothing.
func TestEgressProbeHelperProcess(t *testing.T) {
	if os.Getenv(egressHelperEnv) == "" {
		return
	}
	for i, arg := range os.Args {
		if arg == egressProbeCommand {
			os.Exit(run(context.Background(), os.Args[i:], os.Stdout, os.Stderr))
		}
	}
	os.Exit(3)
}

func TestReplayVerifiesTheSandboxBlocksForgeEgress(t *testing.T) {
	t.Setenv(egressHelperEnv, "1")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := closed.Addr().String()
	_ = closed.Close()

	sandbox := []string{os.Args[0]}
	probe := []string{"-test.run=^TestEgressProbeHelperProcess$", egressProbeCommand}
	passthrough := []string{egressHelperEnv}
	ctx := context.Background()
	if err := verifyEgressBlocked(ctx, sandbox, probe, []string{unreachable}, t.TempDir(), passthrough); err != nil {
		t.Fatalf("sandbox with the forge unreachable: %v", err)
	}
	if err := verifyEgressBlocked(ctx, sandbox, probe, []string{unreachable, listener.Addr().String()}, t.TempDir(), passthrough); err == nil ||
		!strings.Contains(err.Error(), "reachable") {
		t.Fatalf("sandbox reaching a forbidden host verified: %v", err)
	}
	skipping := []string{"-test.run=^$"}
	if err := verifyEgressBlocked(ctx, sandbox, skipping, []string{unreachable}, t.TempDir(), passthrough); err == nil ||
		!strings.Contains(err.Error(), "did not run the egress probe") {
		t.Fatalf("sandbox that never ran the probe verified: %v", err)
	}
	if err := verifyEgressBlocked(ctx, nil, probe, []string{unreachable}, t.TempDir(), passthrough); err == nil {
		t.Fatal("verified without a sandbox")
	}

	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"replay", "-dataset", "d.jsonl", "-reviewer-cmd", "reviewer", "-allow-real-reviewer", "-budget-usd", "1"}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "-sandbox-cmd is required") {
		t.Fatalf("replay without a sandbox exit %d: %s", code, stderr.String())
	}
}

func TestAdaptersRunThroughTheSandbox(t *testing.T) {
	t.Setenv(egressHelperEnv, "1")
	// The sandbox prefix is the helper binary; the adapter argv after it is
	// the egress-probe subcommand, so the probe's token proves the prefix ran.
	adapter := adapterCommand{
		sandbox:     []string{os.Args[0], "-test.run=^TestEgressProbeHelperProcess$"},
		argv:        []string{egressProbeCommand, "127.0.0.1:1"},
		workDir:     t.TempDir(),
		passthrough: []string{egressHelperEnv},
	}
	var out any
	err := adapter.run(context.Background(), "", map[string]string{}, &out)
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("adapter through sandbox = %v; want the probe's non-JSON token to fail decoding", err)
	}
}

func TestEgressProbeChecksTheForgeThroughTheAdapterProxy(t *testing.T) {
	t.Setenv(egressHelperEnv, "1")
	// The forbidden host does not resolve, so only the proxy can reach it;
	// httpproxy never proxies loopback targets, so it cannot be 127.0.0.1.
	const forge = "forge.invalid:443"
	sandbox := []string{os.Args[0]}
	probe := []string{"-test.run=^TestEgressProbeHelperProcess$", egressProbeCommand}
	passthrough := []string{egressHelperEnv}
	for _, tc := range []struct {
		name    string
		status  int
		blocked bool
	}{
		{name: "proxy tunnels to the forge", status: http.StatusOK},
		{name: "proxy refuses the forge", status: http.StatusForbidden, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var connects []string
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodConnect {
					mu.Lock()
					connects = append(connects, r.Host)
					mu.Unlock()
				}
				w.WriteHeader(tc.status)
			}))
			defer proxy.Close()
			t.Setenv("HTTPS_PROXY", proxy.URL)
			t.Setenv("NO_PROXY", "example.com")

			err := verifyEgressBlocked(context.Background(), sandbox, probe, []string{forge}, t.TempDir(), passthrough)
			mu.Lock()
			defer mu.Unlock()
			if len(connects) != 1 || connects[0] != forge {
				t.Fatalf("proxy CONNECTs = %v; want one to %s", connects, forge)
			}
			if tc.blocked {
				if err != nil {
					t.Fatalf("proxy refusing CONNECT: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "reachable through proxy") {
				t.Fatalf("sandbox whose proxy reaches the forge verified: %v", err)
			}
		})
	}
}
