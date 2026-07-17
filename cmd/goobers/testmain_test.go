package main

import (
	"os"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// hermeticEphemeralListen is the address every daemon-lifecycle test binds in
// place of the fixed default, so the OS hands out a free port instead (#798).
const hermeticEphemeralListen = "127.0.0.1:0"

// TestMain arms two whole-suite seams before running any cmd/goobers test:
//
//  1. It neutralizes the harness-preflight seam. These tests drive
//     `goobers up`/`run` against configs that declare agentic stages, but CI has
//     no real, installed Copilot CLI, so the production preflight
//     (LookPath("copilot")) would fail every such test. The real preflight logic
//     is exercised directly in preflight_test.go via preflightAgenticHarnesses,
//     so no coverage is lost by stubbing the wiring seam here.
//
//  2. It makes every daemon-starting test hermetic (#798). A scaffolded instance
//     defaults to instance.DefaultAPIListenAddress (127.0.0.1:8080), so any test
//     that started `goobers up` bound that fixed port — and deterministically
//     collided with the self-host daemon already holding it during a live
//     `go test -race ./cmd/goobers/` run, wedging the whole package. This
//     redirect rewrites ONLY the fixed default to an ephemeral loopback port; a
//     test that deliberately sets its own address (http_lifecycle_test.go's
//     free-port and occupied-port cases) is passed through untouched. It is the
//     path of least resistance — no per-test setup needed — and the structural
//     guard that no test can bind the non-ephemeral default (asserted directly
//     by TestDaemonTestsNeverBindDefaultPort).
func TestMain(m *testing.M) {
	preflightHarnesses = func(map[string]apiv1.GooberSpec, []apiv1.Workflow) error { return nil }

	baseAPIListenAddress := apiListenAddress
	apiListenAddress = func(c *instance.Config) string {
		if addr := baseAPIListenAddress(c); addr != instance.DefaultAPIListenAddress {
			return addr
		}
		return hermeticEphemeralListen
	}

	os.Exit(m.Run())
}
