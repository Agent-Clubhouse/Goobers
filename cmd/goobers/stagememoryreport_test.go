package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func reportLines(t *testing.T, cfg *instance.Config) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	reportStageMemoryBound(cfg, &stdout, &stderr)
	return stdout.String(), stderr.String()
}

// The whole point of the startup report: an instance with no bound is VALID
// and passes every check, so the only way an operator learns the control plane
// is unprotected is the daemon saying so on every start (#4070).
func TestStartupWarnsWhenStagesAreUnbounded(t *testing.T) {
	_, stderr := reportLines(t, &instance.Config{})
	if !strings.Contains(stderr, "UNBOUNDED") {
		t.Errorf("stderr = %q, want the unbounded-stage warning", stderr)
	}
	for _, want := range []string{"OOM-killed", "runner.stageMemoryLimit", "#4070"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("warning does not mention %q:\n%s", want, stderr)
		}
	}
}

// A configured-but-unenforceable bound must NOT read as protection. On a host
// with no delegated cgroup this is the common case, and reporting it as a
// working bound is the failure mode the report exists to prevent.
func TestStartupReportsAConfiguredBoundHonestly(t *testing.T) {
	cfg := &instance.Config{Runner: instance.RunnerConfig{StageMemoryLimit: "8Gi"}}
	stdout, stderr := reportLines(t, cfg)
	combined := stdout + stderr

	if !strings.Contains(combined, "8589934592") {
		t.Errorf("report does not state the bound in bytes:\n%s", combined)
	}
	// Whichever mechanism this host offers, the report must name it — or say
	// plainly that there is none. Silence about the mechanism is the one
	// outcome that is never acceptable.
	named := strings.Contains(combined, "cgroup") ||
		strings.Contains(combined, "RLIMIT_AS") ||
		strings.Contains(combined, "CANNOT BE ENFORCED")
	if !named {
		t.Errorf("report names no enforcement mechanism:\n%s", combined)
	}
	// An unenforceable or proxy-enforced bound is a warning, not a success
	// line: it must not land on stdout looking like protection.
	if strings.Contains(combined, "CANNOT BE ENFORCED") && stderr == "" {
		t.Error("an unenforceable bound was reported without a warning")
	}
}

// A malformed limit enforces nothing. Under --skip-preflight it reaches the
// daemon, and reporting it as a bound would be actively misleading.
func TestStartupWarnsOnAnUnusableLimit(t *testing.T) {
	cfg := &instance.Config{Runner: instance.RunnerConfig{StageMemoryLimit: "eight gigs"}}
	stdout, stderr := reportLines(t, cfg)
	if !strings.Contains(stderr, "unusable") || !strings.Contains(stderr, "unbounded") {
		t.Errorf("stderr = %q, want an unusable-limit warning saying stages are unbounded", stderr)
	}
	if stdout != "" {
		t.Errorf("an unusable limit produced a success line on stdout: %q", stdout)
	}
}

func TestStartupReportToleratesANilConfig(t *testing.T) {
	stdout, stderr := reportLines(t, nil)
	if stdout != "" || stderr != "" {
		t.Errorf("nil config reported %q / %q, want silence", stdout, stderr)
	}
}
