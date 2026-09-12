package main

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

// TestDaemonTestsNeverBindDefaultPort is #798's structural guard: it asserts the
// suite-wide apiListenAddress seam (testmain_test.go) redirects the fixed
// default listen address to an ephemeral loopback port, so no daemon-starting
// test can bind the non-ephemeral default and collide with a co-located
// `goobers up` daemon already holding :8080. An address a test deliberately
// chooses must pass through untouched, so http_lifecycle_test.go's fixed-address
// cases keep working.
func TestDaemonTestsNeverBindDefaultPort(t *testing.T) {
	explicit := "127.0.0.1:54321"
	for _, tc := range []struct {
		name   string
		listen string
		want   string
	}{
		{"default is redirected", instance.DefaultAPIListenAddress, hermeticEphemeralListen},
		{"unset resolves to default and is redirected", "", hermeticEphemeralListen},
		{"explicit address passes through", explicit, explicit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &instance.Config{}
			cfg.API.Listen = tc.listen
			if got := apiListenAddress(cfg); got != tc.want {
				t.Fatalf("apiListenAddress(%q) = %q, want %q (the #798 hermetic seam is misinstalled)", tc.listen, got, tc.want)
			}
		})
	}
}

// TestDaemonStartsWhileDefaultPortOccupied is #798's acceptance criterion as a
// regression test: with something already holding 127.0.0.1:8080 (standing in
// for the live self-host daemon), `goobers up` must still start and drain
// cleanly rather than colliding on the default port and wedging. It exercises
// the seam end to end — the scaffolded instance keeps the default listen, so the
// daemon only starts if the seam has redirected it to an ephemeral port.
func TestDaemonStartsWhileDefaultPortOccupied(t *testing.T) {
	// Occupy the default port ourselves. If it is already held — e.g. by the
	// real co-located daemon this test simulates — that is the exact condition
	// under test, so proceed without our own listener.
	if occupied, err := net.Listen("tcp", instance.DefaultAPIListenAddress); err == nil {
		t.Cleanup(func() { _ = occupied.Close() })
	}

	root := initDeterministicDemo(t)

	var stdout, stderr bytes.Buffer
	if code := runUpThroughStartup(t, []string{root}, &stdout, &stderr); code != 0 {
		t.Fatalf("daemon startup/shutdown code=%d stderr=%q", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "daemon started") {
		t.Fatalf("stdout = %q, want daemon-started message", stdout.String())
	}
	if strings.Contains(stdout.String(), instance.DefaultAPIListenAddress) {
		t.Fatalf("daemon bound the occupied default port instead of an ephemeral one: %q", stdout.String())
	}
}
