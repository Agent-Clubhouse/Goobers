package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/startpin"
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

// TestDirSourcePinsEveryWorkflowOfARealInstance runs the real pin computation
// through the loader on an initialized instance: a generation the control role
// admits must carry digests, or every start is refused as unpinnable.
func TestDirSourcePinsEveryWorkflowOfARealInstance(t *testing.T) {
	root := initDemo(t)
	policy := filepath.Join(root, "instance.yaml")
	data, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, append(data, []byte("placement:\n  selfExecution: deny\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := (cloudconfig.DirSource{Root: root, InstanceID: "instance-a", Pins: startpin.Compute}).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidate.Pins) == 0 || len(candidate.Pins) != len(candidate.Set.Workflows) {
		t.Fatalf("pins = %v for %d workflows, want one per workflow", candidate.Pins, len(candidate.Set.Workflows))
	}
	for key, pins := range candidate.Pins {
		if pins.WorkflowDigest == "" || pins.GooberDigest == "" {
			t.Fatalf("%s has an incomplete pin %+v", key, pins)
		}
	}
}
