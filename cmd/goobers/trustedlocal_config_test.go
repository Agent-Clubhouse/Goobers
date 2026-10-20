package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestTrustedLocalChildConfigRefusesInvalidDirectoryWithDiagnostic(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "invalid.yaml"), []byte("kind: Goober\nmetadata: {name: broken}\nspec: {harness: unknown-harness}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, report, loadErr := instance.LoadConfigDir(directory)
	if !errors.Is(loadErr, instance.ErrInvalidConfig) || report == nil || !report.HasErrors() {
		t.Fatalf("fixture must fail schema validation: %v, %+v", loadErr, report)
	}
	service := &daemonCredentialService{}
	set, err := service.loadTrustedLocalChildConfig(directory)
	if set != nil || !errors.Is(err, instance.ErrInvalidConfig) {
		t.Fatalf("invalid child config admitted: set=%v err=%v", set, err)
	}
	for _, issue := range report.Issues {
		if issue.Severity == validate.Error && !strings.Contains(err.Error(), issue.CLIString()) {
			t.Fatalf("child refusal omitted diagnostic %q: %v", issue.CLIString(), err)
		}
	}
}

func TestTrustedLocalChildConfigJournalsWarningsBeforeAdmission(t *testing.T) {
	root, _, _ := childValidationFixture(t)
	directory := filepath.Join(root, "config")
	warning := validate.Issue{Severity: validate.Warning, Code: validate.WarningModelFallback,
		Kind: "Goober", Name: "coder", Message: "child generation model warning"}
	withValidationIssues(t, warning)
	logDir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(logDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	service := &daemonCredentialService{log: log}
	set, err := service.loadTrustedLocalChildConfig(directory)
	if err != nil || set == nil {
		t.Fatalf("warning should not refuse valid child config: %v", err)
	}
	events, err := journal.ReadInstanceLog(logDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == "config.validation.warning" &&
			event.Runner["code"] == string(warning.Code) && event.Runner["scope"] == "Goober/coder" &&
			event.Runner["severity"] == "warning" && event.Runner["explanation"] == warning.Message {
			found = true
		}
	}
	if !found {
		t.Fatalf("child config warning missing from journal: %+v", events)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if set, err := service.loadTrustedLocalChildConfig(directory); set != nil || !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("child admitted after warning journal failed: set=%v err=%v", set, err)
	}
	service.log = nil
	if set, err := service.loadTrustedLocalChildConfig(directory); set != nil || err == nil {
		t.Fatalf("child admitted without warning journal: set=%v err=%v", set, err)
	}
}
