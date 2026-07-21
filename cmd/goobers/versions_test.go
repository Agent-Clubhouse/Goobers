package main

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/supportmatrix"
)

func TestRunVersionsHuman(t *testing.T) {
	code, stdout, stderr := runArgs(t, "versions")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	for _, want := range []string{
		"goobers support matrix",
		"minimum Go toolchain:",
		supportmatrix.Get().MinGoVersion,
		"supported platforms:",
		"linux/amd64",
		"this host:",
		runtime.GOOS + "/" + runtime.GOARCH,
		runtime.Version(),
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("human output missing %q\n---\n%s", want, stdout)
		}
	}
}

func TestRunVersionsJSON(t *testing.T) {
	code, stdout, stderr := runArgs(t, "versions", "--json")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var got supportmatrix.Report
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n---\n%s", err, stdout)
	}
	if got.MinGoVersion != supportmatrix.Get().MinGoVersion {
		t.Errorf("MinGoVersion = %q, want %q", got.MinGoVersion, supportmatrix.Get().MinGoVersion)
	}
	if len(got.Platforms) != len(supportmatrix.Get().Platforms) {
		t.Errorf("platforms = %d, want %d", len(got.Platforms), len(supportmatrix.Get().Platforms))
	}
	if got.Host != supportmatrix.CurrentHost() {
		t.Errorf("host = %+v, want %+v", got.Host, supportmatrix.CurrentHost())
	}
	// --json is indented for readability, like `version --json`.
	if !strings.Contains(stdout, "\n  ") {
		t.Errorf("JSON output is not indented:\n%s", stdout)
	}
}

func TestRunVersionsRejectsExtraArgs(t *testing.T) {
	code, stdout, _ := runArgs(t, "versions", "bogus")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on usage error", stdout)
	}
}

func TestRunVersionsRejectsUnknownFlag(t *testing.T) {
	code, _, _ := runArgs(t, "versions", "--nope")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}
