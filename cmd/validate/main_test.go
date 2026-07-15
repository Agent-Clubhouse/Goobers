package main

import (
	"bytes"
	"strings"
	"testing"
)

const (
	exampleDir  = "../../config-examples"
	selfhostDir = "../../selfhost"
	badDir      = "../../api/validate/testdata/config-bad"
	validEnv    = "../../api/validate/testdata/envelopes/valid/result.json"
	invalidEnv  = "../../api/validate/testdata/envelopes/invalid/result-bad-status.json"
)

func runArgs(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestRunExampleDirExitsZero(t *testing.T) {
	code, out, _ := runArgs(t, exampleDir)
	if code != 0 {
		t.Fatalf("expected exit 0 for valid example dir, got %d\n%s", code, out)
	}
}

// TestRunSelfhostDirExitsZero guards the self-hosting dogfood config (#28)
// against drift: a future schema/compiler change that breaks it should fail
// CI here, not go unnoticed until someone runs `goobers init` by hand.
func TestRunSelfhostDirExitsZero(t *testing.T) {
	code, out, _ := runArgs(t, selfhostDir)
	if code != 0 {
		t.Fatalf("expected exit 0 for the selfhost config, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "checked 13 object(s) across 13 file(s): 0 error(s), 0 issue(s) total") {
		t.Errorf("expected the full self-hosting object count, got:\n%s", out)
	}
}

func TestRunBadDirExitsOne(t *testing.T) {
	code, out, _ := runArgs(t, badDir)
	if code != 1 {
		t.Fatalf("expected exit 1 for bad dir, got %d", code)
	}
	if !strings.Contains(out, "ghost-gaggle") {
		t.Errorf("expected a cross-reference error in output, got:\n%s", out)
	}
}

func TestRunEnvelopeValidAndInvalid(t *testing.T) {
	if code, _, _ := runArgs(t, "--envelope", "result", validEnv); code != 0 {
		t.Errorf("expected exit 0 for valid envelope, got %d", code)
	}
	if code, _, _ := runArgs(t, "--envelope", "result", invalidEnv); code != 1 {
		t.Errorf("expected exit 1 for invalid envelope, got %d", code)
	}
}

func TestRunUsageErrorExitsTwo(t *testing.T) {
	if code, _, _ := runArgs(t); code != 2 {
		t.Errorf("expected exit 2 when no target given, got %d", code)
	}
	if code, _, _ := runArgs(t, "does-not-exist-dir"); code != 2 {
		t.Errorf("expected exit 2 for missing dir, got %d", code)
	}
}

func TestRunJSONOutput(t *testing.T) {
	code, out, _ := runArgs(t, "--json", exampleDir)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(out, `"issues"`) {
		t.Errorf("expected JSON report with issues field, got:\n%s", out)
	}
}
