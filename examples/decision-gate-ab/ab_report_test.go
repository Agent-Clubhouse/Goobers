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
		intakeAnnotation("github|||acme|widgets|", "1", "2026-10-02T00:00:00Z", "yes", true, false),
		intakeAnnotation("github|||acme|widgets|", "2", "2026-10-02T00:00:00Z", "no", false, false),
		intakeAnnotation("github|||acme|widgets|", "3", "2026-10-02T00:00:00Z", "uncertain", false, true),
	}, []reportTerminal{
		{runID: "run-1", repositoryKey: "github|||acme|widgets|", itemID: "1", status: "escalated", at: "2026-10-02T01:00:00Z"},
		{runID: "run-2", repositoryKey: "github|||acme|widgets|", itemID: "2", status: "completed", at: "2026-10-02T01:00:00Z"},
		{runID: "run-3", repositoryKey: "github|||acme|widgets|", itemID: "3", status: "escalated", at: "2026-10-02T01:00:00Z"},
	})
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

func TestABReportCorrelatesIntakeByRepositoryAndNextTerminal(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh is not installed")
	}
	off := t.TempDir()
	on := t.TempDir()
	writeReportFixture(t, off, nil, nil)
	writeReportFixture(t, on, []string{
		intakeAnnotation("github|||acme|one|", "42", "2026-10-02T00:00:00Z", "yes", true, false),
		intakeAnnotation("github|||acme|two|", "42", "2026-10-02T00:00:00Z", "no", false, false),
		intakeAnnotation("github|||acme|one|", "42", "2026-10-02T02:00:00Z", "no", false, false),
		intakeAnnotation("github|||acme|one|", "42", "2026-10-02T04:00:00Z", "yes", true, false),
	}, []reportTerminal{
		{runID: "one-first", repositoryKey: "github|||acme|one|", itemID: "42", status: "escalated", at: "2026-10-02T01:00:00Z"},
		{runID: "two-first", repositoryKey: "github|||acme|two|", itemID: "42", status: "completed", at: "2026-10-02T01:00:00Z"},
		{runID: "one-second", repositoryKey: "github|||acme|one|", itemID: "42", status: "completed", at: "2026-10-02T03:00:00Z"},
	})
	reportPath := filepath.Join(t.TempDir(), "report.json")

	cmd := exec.Command(pwsh, "-NoProfile", "-File", "ab-report.ps1",
		"-GateOffInstance", off, "-GateOnInstance", on,
		"-MinSample", "30", "-Json", reportPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ab-report.ps1: %v\n%s", err, output)
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
					Unflagged               int `json:"unflagged"`
					UnflaggedLaterEscalated int `json:"unflaggedLaterEscalated"`
				} `json:"backlogIntakeShadow"`
			} `json:"gateOn"`
		} `json:"arms"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	got := report.Arms.GateOn.Intake
	if got.Sampled != 3 || got.Flagged != 1 || got.FlaggedLaterEscalated != 1 ||
		got.Unflagged != 2 || got.UnflaggedLaterEscalated != 0 {
		t.Fatalf("repository/temporal intake summary = %+v", got)
	}
}

type reportTerminal struct {
	runID, repositoryKey, itemID, status, at string
}

func writeReportFixture(t *testing.T, root string, annotations []string, terminals []reportTerminal) {
	t.Helper()
	scheduler := filepath.Join(root, "scheduler")
	if err := os.MkdirAll(scheduler, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, terminal := range terminals {
		annotations = append(annotations, itemRepoAnnotation(terminal))
	}
	if err := os.WriteFile(filepath.Join(scheduler, "events.jsonl"), []byte(strings.Join(annotations, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, terminal := range terminals {
		runDir := filepath.Join(root, "gaggles", "test", "runs", terminal.runID)
		if err := os.MkdirAll(filepath.Join(runDir, "inputs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runDir, "inputs", "item"), []byte(fmt.Sprintf(`{"id":%q}`, terminal.itemID)), 0o644); err != nil {
			t.Fatal(err)
		}
		event := fmt.Sprintf(`{"time":%q,"type":"run.finished","status":%q}`, terminal.at, terminal.status)
		if err := os.WriteFile(filepath.Join(runDir, "events.jsonl"), []byte(event), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func intakeAnnotation(repositoryKey, itemID, at, verdict string, flagged, errored bool) string {
	return fmt.Sprintf(
		`{"time":%q,"type":"runner.annotation","runner":{"annotation":"backlog.intake-decision-shadow","provider":"github","repositoryKey":%q,"itemId":%q,"verdict":%q,"flagged":%t,"error":%t}}`,
		at, repositoryKey, itemID, verdict, flagged, errored,
	)
}

func itemRepoAnnotation(terminal reportTerminal) string {
	return fmt.Sprintf(
		`{"time":%q,"type":"runner.annotation","runId":%q,"runner":{"annotation":"item-repo","itemId":%q,"repositoryKey":%q}}`,
		terminal.at, terminal.runID, terminal.itemID, terminal.repositoryKey,
	)
}
