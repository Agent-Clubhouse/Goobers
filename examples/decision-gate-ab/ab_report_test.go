package decisiongateab

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestABReportCorrelatesIntakeShadowWithTerminalRuns(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh is not installed")
	}
	off := t.TempDir()
	on := t.TempDir()
	writeReportFixture(t, off, nil, nil)
	writeReportFixture(t, on, []string{
		intakeAnnotation("1", "yes", true, false),
		intakeAnnotation("2", "no", false, false),
		intakeAnnotation("3", "uncertain", false, true),
	}, map[string]string{"1": "escalated", "2": "completed", "3": "escalated"})
	reportPath := filepath.Join(t.TempDir(), "report.json")

	cmd := exec.Command(pwsh, "-NoProfile", "-File", "ab-report.ps1",
		"-GateOffInstance", off, "-GateOnInstance", on,
		"-MinSample", "4", "-Json", reportPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ab-report.ps1: %v\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{
		"Intake shadow sampled", "3",
		"Intake shadow flagged", "1 (n=2)",
		"... flagged later escalated", "1 (n=1)",
		"... unflagged later escalated", "0 (n=1)",
		"Intake shadow uncertain/error", "1 (n=3)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
	if regexp.MustCompile(`\(\d+(?:\.\d+)?%\)`).MatchString(text) {
		t.Fatalf("report emitted a below-threshold rate:\n%s", text)
	}

	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Arms struct {
			GateOn struct {
				Intake struct {
					Sampled                 int `json:"sampled"`
					Flagged                 int `json:"flagged"`
					FlaggedLaterEscalated   int `json:"flaggedLaterEscalated"`
					UnflaggedLaterEscalated int `json:"unflaggedLaterEscalated"`
					UncertainOrError        int `json:"uncertainOrError"`
				} `json:"backlogIntakeShadow"`
			} `json:"gateOn"`
		} `json:"arms"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	got := report.Arms.GateOn.Intake
	if got.Sampled != 3 || got.Flagged != 1 || got.FlaggedLaterEscalated != 1 ||
		got.UnflaggedLaterEscalated != 0 || got.UncertainOrError != 1 {
		t.Fatalf("intake summary = %+v", got)
	}
}

func writeReportFixture(t *testing.T, root string, annotations []string, terminals map[string]string) {
	t.Helper()
	scheduler := filepath.Join(root, "scheduler")
	if err := os.MkdirAll(scheduler, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scheduler, "events.jsonl"), []byte(strings.Join(annotations, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	for itemID, status := range terminals {
		runDir := filepath.Join(root, "gaggles", "test", "runs", "run-"+itemID)
		if err := os.MkdirAll(filepath.Join(runDir, "inputs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runDir, "inputs", "item"), []byte(fmt.Sprintf(`{"id":%q}`, itemID)), 0o644); err != nil {
			t.Fatal(err)
		}
		event := fmt.Sprintf(`{"time":"2026-10-02T01:00:00Z","type":"run.finished","status":%q}`, status)
		if err := os.WriteFile(filepath.Join(runDir, "events.jsonl"), []byte(event), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func intakeAnnotation(itemID, verdict string, flagged, errored bool) string {
	return fmt.Sprintf(
		`{"time":"2026-10-02T00:00:00Z","type":"runner.annotation","runner":{"annotation":"backlog.intake-decision-shadow","itemId":%q,"verdict":%q,"flagged":%t,"error":%t}}`,
		itemID, verdict, flagged, errored,
	)
}
