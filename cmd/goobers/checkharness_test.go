package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
)

// withHarnessAdapter substitutes harnessAdapterFor for the duration of a
// test, so --check-harness tests never depend on a real, installed,
// signed-in Copilot CLI being present on the machine running `make ci`.
func withHarnessAdapter(t *testing.T, lookup func(apiv1.Harness) (harness.Adapter, error)) {
	t.Helper()
	orig := harnessAdapterFor
	harnessAdapterFor = lookup
	t.Cleanup(func() { harnessAdapterFor = orig })
}

func TestAdapterForKnownAndUnknownHarness(t *testing.T) {
	adapter, err := adapterFor(apiv1.HarnessCopilot)
	if err != nil {
		t.Fatalf("adapterFor(copilot): %v", err)
	}
	if _, ok := adapter.(*harness.CopilotAdapter); !ok {
		t.Fatalf("adapterFor(copilot) = %T, want *harness.CopilotAdapter", adapter)
	}

	if _, err := adapterFor("claude-code"); err == nil {
		t.Fatal("expected an error for an unsupported harness")
	}
}

func TestCheckHarnessesSucceeds(t *testing.T) {
	withHarnessAdapter(t, func(h apiv1.Harness) (harness.Adapter, error) {
		return &harness.FakeAdapter{AdapterName: string(h)}, nil
	})
	goobers := []apiv1.Goober{
		{Spec: apiv1.GooberSpec{Harness: apiv1.HarnessCopilot}},
	}
	var out, errOut strings.Builder
	if !checkHarnesses(goobers, &out, &errOut) {
		t.Fatalf("checkHarnesses returned false; stdout=%q", out.String())
	}
	if !strings.Contains(out.String(), "HARNESS copilot: OK") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestCheckHarnessesFailsClosedOnPreflightError(t *testing.T) {
	withHarnessAdapter(t, func(h apiv1.Harness) (harness.Adapter, error) {
		return &harness.FakeAdapter{PreflightErr: errNotSignedIn}, nil
	})
	goobers := []apiv1.Goober{
		{Spec: apiv1.GooberSpec{Harness: apiv1.HarnessCopilot}},
	}
	var out, errOut strings.Builder
	if checkHarnesses(goobers, &out, &errOut) {
		t.Fatal("checkHarnesses returned true, want false on a Preflight failure")
	}
	if !strings.Contains(out.String(), errNotSignedIn.Error()) {
		t.Fatalf("stdout = %q, want it to include the actionable guidance", out.String())
	}
}

func TestCheckHarnessesDedupsRepeatedHarness(t *testing.T) {
	calls := 0
	withHarnessAdapter(t, func(h apiv1.Harness) (harness.Adapter, error) {
		calls++
		return &harness.FakeAdapter{AdapterName: string(h)}, nil
	})
	goobers := []apiv1.Goober{
		{Spec: apiv1.GooberSpec{Harness: apiv1.HarnessCopilot}},
		{Spec: apiv1.GooberSpec{Harness: apiv1.HarnessCopilot}},
		{Spec: apiv1.GooberSpec{Harness: ""}}, // no harness declared — skipped
	}
	var out, errOut strings.Builder
	if !checkHarnesses(goobers, &out, &errOut) {
		t.Fatal("checkHarnesses returned false")
	}
	if calls != 1 {
		t.Fatalf("harnessAdapterFor called %d times, want 1 (dedup by harness kind)", calls)
	}
}

var errNotSignedIn = harnessTestErr("copilot-cli: not signed in — run `copilot` once interactively to authenticate")

type harnessTestErr string

func (e harnessTestErr) Error() string { return string(e) }

// TestValidateCheckHarnessFlagWiring exercises the full CLI path end to end
// with a fake adapter substituted in, proving --check-harness is actually
// wired into `goobers validate` (not just unit-testable in isolation).
func TestValidateCheckHarnessFlagWiring(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code=%d stderr=%q", code, stderr)
	}

	withHarnessAdapter(t, func(h apiv1.Harness) (harness.Adapter, error) {
		return &harness.FakeAdapter{AdapterName: string(h)}, nil
	})
	code, stdout, stderr := runArgs(t, "validate", "--check-harness", root)
	if code != 0 {
		t.Fatalf("validate --check-harness: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "HARNESS copilot: OK") {
		t.Fatalf("stdout = %q", stdout)
	}

	withHarnessAdapter(t, func(h apiv1.Harness) (harness.Adapter, error) {
		return &harness.FakeAdapter{PreflightErr: errNotSignedIn}, nil
	})
	code, stdout, _ = runArgs(t, "validate", "--check-harness", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 on a failed harness preflight; stdout=%q", code, stdout)
	}
}

// authProbeFakeRunner scripts a CopilotAdapter's two preflight subprocesses
// separately: the --version check vs the -p auth probe (identified by the "-p"
// flag AuthCheckArgs carries), so a signed-out CLI (version OK, auth fails) can
// be simulated without a real copilot binary.
type authProbeFakeRunner struct {
	versionExit, authExit int
	authInvoked           *bool
}

func (r *authProbeFakeRunner) Run(_ context.Context, req harness.ProcessRequest) (harness.ProcessResult, error) {
	for _, a := range req.Command {
		if a == "-p" { // the auth probe (copilotAuthCheckArgs starts with -p)
			if r.authInvoked != nil {
				*r.authInvoked = true
			}
			return harness.ProcessResult{ExitCode: r.authExit}, nil
		}
	}
	return harness.ProcessResult{ExitCode: r.versionExit}, nil // the --version check
}

// TestCheckHarnessesRunsAuthProbe is the #284/#271 control: --check-harness
// probes authentication, so a CLI that passes --version but fails the auth probe
// (a signed-out / mis-scoped token) fails closed, and a fully-authenticated one
// passes. Command "echo" is on PATH so LookPath succeeds and the scripted runner
// governs the result.
func TestCheckHarnessesRunsAuthProbe(t *testing.T) {
	goobers := []apiv1.Goober{{Spec: apiv1.GooberSpec{Harness: apiv1.HarnessCopilot}}}

	// Signed out: version OK, auth probe fails → check fails, and the probe ran.
	authInvoked := false
	withHarnessAdapter(t, func(apiv1.Harness) (harness.Adapter, error) {
		return &harness.CopilotAdapter{
			Command: []string{"echo"},
			Runner:  &authProbeFakeRunner{versionExit: 0, authExit: 1, authInvoked: &authInvoked},
		}, nil
	})
	var out, errOut strings.Builder
	if checkHarnesses(goobers, &out, &errOut) {
		t.Fatal("checkHarnesses returned true; a signed-out CLI (version OK, auth probe fails) must fail closed")
	}
	if !authInvoked {
		t.Fatal("the auth probe (-p) was never invoked — AuthCheckArgs not wired into --check-harness")
	}

	// Fully authenticated: both succeed → check passes.
	withHarnessAdapter(t, func(apiv1.Harness) (harness.Adapter, error) {
		return &harness.CopilotAdapter{
			Command: []string{"echo"},
			Runner:  &authProbeFakeRunner{versionExit: 0, authExit: 0},
		}, nil
	})
	out.Reset()
	errOut.Reset()
	if !checkHarnesses(goobers, &out, &errOut) {
		t.Fatalf("checkHarnesses returned false for a healthy signed-in CLI; stdout=%q", out.String())
	}
}
