package instance

import (
	"errors"
	"testing"
)

const (
	validConfigDir = "../../config-examples"
	badConfigDir   = "../../api/validate/testdata/config-bad"
)

func TestLoadConfigDirValid(t *testing.T) {
	set, report, err := LoadConfigDir(validConfigDir)
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	if set.Manifest == nil {
		t.Fatal("expected a Manifest")
	}
	if len(set.Gaggles) != 1 || set.Gaggles[0].Name != "acme-web" {
		t.Fatalf("unexpected gaggles: %+v", set.Gaggles)
	}
	if len(set.Goobers) != 1 || set.Goobers[0].Name != "coder" {
		t.Fatalf("unexpected goobers: %+v", set.Goobers)
	}
	if len(set.Workflows) != 1 || set.Workflows[0].Name != "default-implement" {
		t.Fatalf("unexpected workflows: %+v", set.Workflows)
	}
}

func TestLoadConfigDirInvalid(t *testing.T) {
	set, report, err := LoadConfigDir(badConfigDir)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
	if set != nil {
		t.Fatalf("expected a nil ConfigSet on invalid config, got %+v", set)
	}
	if report == nil || !report.HasErrors() {
		t.Fatalf("expected a report with errors, got %+v", report)
	}
}

func TestLoadConfigDirMissingDir(t *testing.T) {
	_, _, err := LoadConfigDir("../../does/not/exist")
	if err == nil {
		t.Fatal("expected an error for a missing config directory")
	}
}
