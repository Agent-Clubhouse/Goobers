package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/validate"
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
	if !strings.Contains(out, "checked 14 object(s) across 14 file(s): 0 error(s), 0 issue(s) total") {
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
	const want = "{\n  \"issues\": null,\n  \"files\": 13,\n  \"objects\": 13\n}\n"
	if out != want {
		t.Errorf("JSON output = %q, want %q", out, want)
	}
}

func TestRunWorkflowWarningPreservesCLIOutput(t *testing.T) {
	dir := warningConfigDir(t)
	const want = `WARNING Workflow/implementation: task "query-backlog" runs backlog-query --claim without inputs.resultFile; empty ticks will report success instead of no-work`

	code, out, _ := runArgs(t, dir)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", code, out)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("output missing legacy warning:\n%s", out)
	}
	if strings.Contains(out, "VER003") || strings.Contains(out, "Gaggle/acme-web") ||
		strings.Contains(out, "gaggles/acme-web/workflows/implementation.yaml") {
		t.Fatalf("output exposed API warning provenance:\n%s", out)
	}

	code, out, _ = runArgs(t, "--json", dir)
	if code != 0 {
		t.Fatalf("expected JSON exit 0, got %d\n%s", code, out)
	}
	var report validate.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out)
	}
	// Two warnings, both consequences of the one stripped resultFile line and
	// both true: the no-work one is about empty ticks reporting success, the
	// inert one is about query-backlog's expectedOutputs having no channel to
	// emit through. Neither may leak API provenance, which is what this test
	// actually guards.
	if len(report.Issues) != 2 {
		t.Fatalf("JSON issues = %+v, want two warnings", report.Issues)
	}
	for _, issue := range report.Issues {
		if issue.Code != "" || issue.File != "" || issue.Gaggle != "" {
			t.Fatalf("JSON warning exposed API provenance: %+v", issue)
		}
	}
}

func warningConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(exampleDir)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gaggles", "acme-web", "workflows", "implementation.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const resultFile = `        resultFile: "claimed-item.json"`
	updated := strings.Replace(string(raw), resultFile, "", 1)
	if updated == string(raw) {
		t.Fatal("workflow fixture did not contain resultFile")
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}
