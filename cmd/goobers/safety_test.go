package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/workflowsafety"
)

func safetyDemo(t *testing.T) (string, string) {
	t.Helper()
	root := initDeterministicDemo(t)
	instancePath := filepath.Join(root, "instance.yaml")
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	path := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, instancePath, "your-org", "acme")
	replaceInFile(t, instancePath, "your-repo", "widgets")
	for range 2 {
		replaceInFile(t, gagglePath, "your-org", "acme")
		replaceInFile(t, gagglePath, "your-repo", "widgets")
	}
	replaceInFile(t, path, `command: ["true"]`, `command: ["never-execute-safety-analysis"]`)
	return root, path
}

func TestSafetyCompatibilityFilterPreservesOtherOutput(t *testing.T) {
	for _, code := range workflowsafety.Codes() {
		output := "before\nWARNING " + code + " advisory\nafter\n"
		if got := withoutSafetyWarnings(output); got != "before\nafter\n" {
			t.Fatalf("%s filter = %q", code, got)
		}
	}
	for _, output := range []string{
		"WARNING SAF999 unknown\n",
		"WARNING MODEL002 legacy\n",
		"ERROR SAF006 failure\n",
		"WARNING SAF006extra not-a-safety-code\n",
	} {
		if got := withoutSafetyWarnings(output); got != output {
			t.Fatalf("filter discarded non-advisory output: %q -> %q", output, got)
		}
	}
}

func TestSafetyStatusCollapsedWarningsRemainInspectable(t *testing.T) {
	var manual apiv1.Workflow
	manual.Name = "shared-name"
	manual.Spec.Gaggle = "manual"
	manual.Spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerManual}}
	scheduled := manual
	scheduled.Spec.Gaggle = "scheduled"
	scheduled.Spec.Triggers = nil
	workflows := []apiv1.Workflow{manual, scheduled}
	warnings := []validate.CodedWarning{
		{
			Code: workflowsafety.CoverageCode,
			Safety: &workflowsafety.Details{
				Gaggle: manual.Spec.Gaggle, Workflow: manual.Name,
			},
		},
		{
			Code: workflowsafety.CoverageCode,
			Safety: &workflowsafety.Details{
				Gaggle: scheduled.Spec.Gaggle, Workflow: scheduled.Name,
			},
		},
		{Code: validate.WarningModelFallback, Scope: "Workflow/shared-name"},
	}
	collapsed := statusTextWarnings(warnings, workflows, 1, false, "")
	if len(collapsed) != 3 || collapsed[0].Scope != "Workflows" ||
		!reflect.DeepEqual(collapsed[1:], warnings[1:]) {
		t.Fatalf("default status hid unrelated warnings or expanded manual details: %+v", collapsed)
	}
	for _, tc := range []struct {
		all      bool
		selected string
	}{
		{all: true},
		{selected: manual.Name},
	} {
		got := statusTextWarnings(warnings, workflows, 0, tc.all, tc.selected)
		if !reflect.DeepEqual(got, warnings) {
			t.Fatalf("all=%v selected=%q: warnings = %+v, want %+v", tc.all, tc.selected, got, warnings)
		}
	}
}

func TestSafetyCLIStartupAndLoaderUseIdenticalAdvisories(t *testing.T) {
	root, workflowPath := safetyDemo(t)
	marker := filepath.Join(t.TempDir(), "executed")
	command, err := json.Marshal([]string{os.Args[0], "-test.run=^TestSafetyExecutionSentinel$", "safety-sentinel", marker})
	if err != nil {
		t.Fatal(err)
	}
	replaceInFile(t, workflowPath, `["never-execute-safety-analysis"]`, string(command))
	_, report, err := instance.LoadConfigDir(filepath.Join(root, "config"))
	if err != nil {
		t.Fatal(err)
	}
	var expected *workflowsafety.Details
	for _, issue := range report.Issues {
		if issue.Code == workflowsafety.CoverageCode {
			expected = issue.Safety
		}
	}
	if expected == nil {
		t.Fatal("loader omitted safety advisory")
	}
	for _, command := range []string{"validate", "lint"} {
		for _, strict := range []bool{false, true} {
			args := []string{command, "--json", "--github-annotations"}
			if strict {
				args = append(args, "--strict")
			}
			args = append(args, root)
			code, stdout, stderr := runArgs(t, args...)
			if code != 0 {
				t.Fatalf("%v exit=%d stdout=%s stderr=%s", args, code, stdout, stderr)
			}
			assertDiagnosticsSchema(t, stdout)
			envelope := decodeDiagnosticsEnvelope(t, stdout)
			var found bool
			for _, finding := range envelope.Findings {
				if finding.Code == workflowsafety.CoverageCode {
					found = true
					if !reflect.DeepEqual(finding.Safety, expected) || finding.Line == 0 {
						t.Fatalf("CLI/loader safety disagreement: %+v vs %+v", finding.Safety, expected)
					}
				}
			}
			if !found || !strings.Contains(stderr, "::warning file=") || !strings.Contains(stderr, workflowsafety.CoverageCode) {
				t.Fatalf("missing JSON/annotation finding: %s %s", stdout, stderr)
			}
		}
	}
	if code := runStartupConfigPreflight(root, false, io.Discard); code != 0 {
		t.Fatalf("advisory blocked daemon startup: %d", code)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("semantic validation executed the workflow command: %v", err)
	}
}

func TestSafetyExecutionSentinel(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "safety-sentinel" {
		t.Skip("subprocess-only sentinel")
	}
	if err := os.WriteFile(os.Args[len(os.Args)-1], []byte("executed"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSafetyDaemonWarningsClearAfterAcceptedReload(t *testing.T) {
	root, workflowPath := safetyDemo(t)
	layout := instance.NewLayout(root)
	address := freeLoopbackAddress(t)
	setAPIListenAddress(t, root, address)
	previousInterval := configReloadInterval
	configReloadInterval = 20 * time.Millisecond
	t.Cleanup(func() { configReloadInterval = previousInterval })
	ctx, cancel := context.WithCancel(context.Background())
	started := &daemonStartedWriter{started: make(chan struct{})}
	done := make(chan int, 1)
	go func() { done <- runUpContext(ctx, []string{"--quiet", root}, started, io.Discard) }()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("daemon exit=%d", code)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	select {
	case <-started.started:
	case <-time.After(15 * time.Second):
		t.Fatal("daemon with advisory did not reach readiness")
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, event := range events {
		if event.Runner["code"] == workflowsafety.CoverageCode {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("startup safety warnings=%d, want one", count)
	}
	initial := readDaemonHealth(t, address)
	if !initial.Ready {
		t.Fatal("advisory changed readiness")
	}
	before := readSafetyWarnings(t, address)
	if len(before) != 1 || before[0].Safety == nil {
		t.Fatalf("advisory unavailable through persistent instance API: %+v", before)
	}
	replaceInFile(t, workflowPath, `command: ["never-execute-safety-analysis"]`, `command: ["true"]`)
	waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloaded, 1)
	waitForDefinitionsReload(t, address, initial.Freshness.DefinitionsLoadedAt)
	_, repaired, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, warning := range repaired.Warnings() {
		if warning.Code == workflowsafety.CoverageCode {
			t.Fatalf("repair did not clear advisory: %+v", warning)
		}
	}
	if !readDaemonHealth(t, address).Ready {
		t.Fatal("repair changed readiness")
	}
	if got := readSafetyWarnings(t, address); len(got) != 0 {
		t.Fatalf("applied API still exposes repaired findings: %+v", got)
	}

	repairedAt := readDaemonHealth(t, address).Freshness.DefinitionsLoadedAt
	replaceInFile(t, workflowPath, `command: ["true"]`, `command: ["never-execute-safety-analysis"]`)
	waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloaded, 2)
	waitForDefinitionsReload(t, address, repairedAt)
	if got := readSafetyWarnings(t, address); !reflect.DeepEqual(got, before) {
		t.Fatalf("accepted reload/startup findings differ: before=%+v after=%+v", before, got)
	}
	valid, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, []byte("invalid: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloadRejected, 1)
	waitForConfigValue(t, "rejected candidate health", func() (bool, bool) {
		health := readDaemonHealth(t, address)
		return true, health.DefinitionReload != nil && health.DefinitionReload.State == "rejected" &&
			health.DefinitionReload.RejectionReason != ""
	})
	if got := readSafetyWarnings(t, address); !reflect.DeepEqual(got, before) {
		t.Fatalf("rejected candidate replaced applied advisories: %+v", got)
	}
	if !readDaemonHealth(t, address).Ready {
		t.Fatal("candidate rejection changed applied readiness")
	}
	if err := os.WriteFile(workflowPath, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForConfigValue(t, "restored applied config", func() (bool, bool) {
		health := readDaemonHealth(t, address)
		return true, health.DefinitionReload != nil && health.DefinitionReload.State == "current"
	})
	events, err = journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	count = 0
	for _, event := range events {
		if event.Runner["code"] == workflowsafety.CoverageCode {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected startup and one accepted-reload advisory, not per-tick spam; got %d", count)
	}
}

func readSafetyWarnings(t *testing.T, address string) []validate.CodedWarning {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get("http://" + address + httpapi.InstancePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close instance response: %v", err)
		}
	}()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("instance API status=%d", response.StatusCode)
	}
	var instance readservice.Instance
	if err := json.NewDecoder(response.Body).Decode(&instance); err != nil {
		t.Fatal(err)
	}
	var warnings []validate.CodedWarning
	for _, warning := range instance.Warnings {
		if strings.HasPrefix(string(warning.Code), "SAF") {
			warnings = append(warnings, warning)
		}
	}
	return warnings
}

func TestSafetySiblingBindingCatalogMatchesBuiltin(t *testing.T) {
	for _, head := range []string{"goobers/implementation/run-10", "feature/external-review"} {
		t.Run(head, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(10, "Selected PR")
			server.addOpenPR(10, head, "main", "sha10", "base", false, nil, nil)
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
			t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
			dir := t.TempDir()
			t.Chdir(dir)
			argv := []string{"goobers", "gather-sibling-context", "--no-verdict-cache"}
			effects := workflowsafety.CommandEffects(apiv1.Task{
				Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: argv},
			})
			if !effects.ConditionalRebind || effects.Rebinds {
				t.Fatalf("catalog must distinguish conditional rebinding: %+v", effects)
			}
			if code, stdout, stderr := runArgs(t, append(argv[1:], root)...); code != 0 {
				t.Fatalf("builtin code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "sibling-context.json"))
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				WorkspaceBranch string `json:"workspaceBranch"`
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			want := ""
			if strings.HasPrefix(head, "goobers/") {
				want = head
			}
			if result.WorkspaceBranch != want {
				t.Fatalf("builtin branch=%q, catalog expects %q", result.WorkspaceBranch, want)
			}
		})
	}
}

func TestSafetyRejectedCandidateRemainsSeparate(t *testing.T) {
	root, _ := safetyDemo(t)
	layout := instance.NewLayout(root)
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := log.Close(); err != nil {
			t.Errorf("close instance journal: %v", err)
		}
	}()
	reloader := &configReloader{setup: &schedulerSetup{InstanceLog: log},
		appliedDigest: "applied", observedDigest: "candidate", watching: true}
	report := &validate.Report{Issues: []validate.Issue{{Code: workflowsafety.EvidenceCode,
		Severity: validate.Warning, Message: "candidate-only finding"}}}
	if err := reloader.reject("candidate", &configReportError{report: report, err: instance.ErrInvalidConfig}); err != nil {
		t.Fatal(err)
	}
	reloader.lastRejectionMessage = ""
	status := reloader.reloadStatus(time.Now())
	if status.State != "rejected" || status.RejectionReason == "" ||
		len(status.CandidateWarnings) != 1 || status.AppliedDigest != "applied" {
		t.Fatalf("candidate diagnostics lost or mixed with applied config: %+v", status)
	}
	reloader.observedDigest = "applied"
	status = reloader.reloadStatus(time.Now())
	if status.State != "current" || len(status.CandidateWarnings) != 0 || status.RejectionReason != "" {
		t.Fatalf("resolved candidate warnings remain active: %+v", status)
	}
}
