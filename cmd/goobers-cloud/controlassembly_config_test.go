package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/schedentries"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
)

func TestConfigurationConstructorFailsClosedWithoutItsDependencies(t *testing.T) {
	owner := func() *lease.Handle { return nil }
	manager := &lease.Manager{}
	for name, tc := range map[string]struct {
		db      databaseServices
		manager *lease.Manager
		owner   func() *lease.Handle
		want    string
	}{
		"no config directory": {db: databaseServices{InstanceID: "instance-a"}, manager: manager, owner: owner, want: "--config-dir"},
		"no object store":     {db: databaseServices{InstanceID: "instance-a", ConfigDir: t.TempDir()}, manager: manager, owner: owner, want: "--object-store-url"},
	} {
		t.Run(name, func(t *testing.T) {
			var planes controlPlanes
			err := constructConfiguration(t.Context(), tc.db, controlAuth{}, tc.manager, tc.owner, &planes)
			if err == nil || !strings.Contains(err.Error(), tc.want) || planes.Configuration != nil {
				t.Fatalf("construct = %v (configuration %v), want a refusal naming %q", err, planes.Configuration != nil, tc.want)
			}
		})
	}
}

func TestLeasedActivatorRefusesToPublishWithoutALiveHandle(t *testing.T) {
	activator := leasedActivator{manager: &lease.Manager{}, owner: func() *lease.Handle { return nil }}
	if err := activator.Activate(t.Context(), cloudconfig.Candidate{Digest: "sha256:x"}); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("Activate without a live handle = %v, want ErrStale", err)
	}
}

func TestControlConfigurationDirectoryFailsClosedAtBoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"unset": "", "absent": filepath.Join(t.TempDir(), "absent"), "a file": file} {
		var db databaseServices
		if err := bindControlConfiguration(&db, dir); err == nil || db.ConfigDir != "" {
			t.Fatalf("%s: bound %q (%v)", name, db.ConfigDir, err)
		}
	}
	var db databaseServices
	if err := bindControlConfiguration(&db, t.TempDir()); err != nil || db.ConfigDir == "" {
		t.Fatalf("a real directory was refused: %v", err)
	}
}

func TestConfigurationIsAControllerSideSeamWithAConstructor(t *testing.T) {
	for _, seam := range controlAssembly {
		if seam.field != "Configuration" {
			continue
		}
		if !seam.controller || seam.construct == nil {
			t.Fatalf("configuration seam = %+v, want a controller-side constructor", seam)
		}
		return
	}
	t.Fatal("no Configuration seam")
}

func TestControlCommandRequiresAConfigDirectory(t *testing.T) {
	t.Setenv("GOOBERS_CONFIG_DIR", "")
	for _, args := range [][]string{{"control", "--help"}} {
		code, stdout, _ := runForTest(args...)
		if code != 0 || !strings.Contains(stdout, "--config-dir") || !strings.Contains(stdout, "GOOBERS_CONFIG_DIR") {
			t.Fatalf("help omits the config directory: %d %s", code, stdout)
		}
	}
}

func TestLeasedGenerationsRefuseToPinWithoutALiveHandle(t *testing.T) {
	source := leasedGenerations{owner: func() *lease.Handle { return nil }}
	if _, err := source.Acquire(t.Context(), "sha256:x"); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("Acquire without a live handle = %v, want ErrStale", err)
	}
}

// TestAnUnparseableTriggerRefusesTheGenerationAtApplyTime proves the composed
// assessor refuses a generation the cloud scheduler could not tick, in the
// config status shape: the offending workflow is named with its finding.
func TestAnUnparseableTriggerRefusesTheGenerationAtApplyTime(t *testing.T) {
	cfg := &instance.Config{Placement: &instance.PlacementConfig{SelfExecution: "deny"}}
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "g"}}},
		Workflows: []apiv1.Workflow{{
			ObjectMeta: metav1.ObjectMeta{Name: "nightly"}, DSLVersion: supportmatrix.V3DSLVersion,
			Spec: apiv1.WorkflowSpec{
				Gaggle: "g", Start: "t",
				Tasks:    []apiv1.Task{{Name: "t", Type: apiv1.TaskDeterministic, Goal: "run", Run: &apiv1.DeterministicRun{Command: []string{"true"}}}},
				Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "not a cron"}},
			},
		}},
	}
	refusal := cloudconfig.Admit(configurationAssessor, "sha256:x", cfg, set)
	if refusal == nil {
		t.Fatal("a generation with an unparseable cron trigger was admitted")
	}
	findings := refusal.Workflows["g/nightly"]
	if len(findings) == 0 || findings[len(findings)-1].Code != schedentries.CodeTrigger {
		t.Fatalf("refusal = %+v, want TRIGGER001 on g/nightly", refusal.Workflows)
	}
}
