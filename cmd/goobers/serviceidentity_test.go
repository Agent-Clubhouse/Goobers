package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	daemonservice "github.com/goobers/goobers/internal/service"
)

type identityTaskManager struct{ *fakeDaemonServiceManager }

func (m identityTaskManager) InstallTask(ctx context.Context) (daemonservice.Status, error) {
	return m.Install(ctx)
}
func (m identityTaskManager) StartTask(ctx context.Context) (daemonservice.Status, error) {
	return m.Start(ctx)
}
func (m identityTaskManager) StopTask(ctx context.Context) error      { return m.Stop(ctx) }
func (m identityTaskManager) UninstallTask(ctx context.Context) error { return m.Uninstall(ctx) }
func (m identityTaskManager) TaskStatus(ctx context.Context) (daemonservice.Status, error) {
	return m.Status(ctx)
}

func TestServiceMutationIdentityGuards(t *testing.T) {
	commands := []struct {
		name    string
		run     func([]string, io.Writer, io.Writer) int
		cleanup bool
	}{
		{"install", runServiceInstall, false},
		{"start", runServiceStart, false},
		{"stop", runServiceStop, true},
		{"uninstall", runServiceUninstall, true},
		{"task-install", runServiceTaskInstall, false},
		{"task-start", runServiceTaskStart, false},
		{"task-stop", runServiceTaskStop, true},
		{"task-uninstall", runServiceTaskUninstall, true},
	}
	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			root := serviceTestInstance(t)
			manager := &fakeDaemonServiceManager{status: daemonservice.Status{Installed: true}}
			useFakeDaemonServiceManager(t, manager)
			previous := newScheduledTaskManager
			newScheduledTaskManager = func(string) (scheduledTaskManager, error) { return identityTaskManager{manager}, nil }
			t.Cleanup(func() { newScheduledTaskManager = previous })
			if code := command.run([]string{root}, io.Discard, brokenRootBannerWriter{}); code != 2 {
				t.Fatalf("broken display accepted: %d", code)
			}
			if manager.installed || manager.started || manager.stopped || manager.uninstalled {
				t.Fatal("service mutation preceded successful display")
			}
			if _, err := instance.DecommissionRoot(context.Background(), root, "migrated", time.Now()); err != nil {
				t.Fatal(err)
			}
			var stderr strings.Builder
			code := command.run([]string{root}, io.Discard, &stderr)
			if command.cleanup {
				if code != 0 || !strings.Contains(stderr.String(), "Historical root; do not use") || (!manager.stopped && !manager.uninstalled) {
					t.Fatalf("historical cleanup blocked or unidentified: %d %s", code, stderr.String())
				}
			} else if code != 2 || manager.installed || manager.started || !strings.Contains(stderr.String(), "historical root; do not use") {
				t.Fatalf("historical activation accepted: %d %s", code, stderr.String())
			}
		})
	}
}

func TestServiceSupervisorRefusesHistoricalRootBeforePlatformSetup(t *testing.T) {
	root := serviceTestInstance(t)
	if _, err := instance.DecommissionRoot(context.Background(), root, "migrated", time.Now()); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	// Unset dependencies deliberately fail if any platform or supervisor work
	// is reached before the root lifecycle refusal.
	code := runServiceSuperviseWith([]string{root}, io.Discard, &stderr, serviceSuperviseDeps{})
	if code != 2 || !strings.Contains(stderr.String(), "historical root; do not use") {
		t.Fatalf("historical supervisor activated: %d %s", code, stderr.String())
	}
}
