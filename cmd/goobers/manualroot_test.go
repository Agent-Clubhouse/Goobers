package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

type brokenRootBannerWriter struct{}

func (brokenRootBannerWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestMaintenanceRootPreviewDoesNotAdoptLegacyIdentity(t *testing.T) {
	root := initScheduledDemo(t)
	if err := os.Remove(filepath.Join(root, instance.RootIdentityFileName)); err != nil {
		t.Fatal(err)
	}
	var banner bytes.Buffer
	if err := prepareMaintenanceRoot(instance.NewLayout(root), true, &banner); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(banner.String(), "Legacy root") {
		t.Fatalf("legacy warning absent: %s", banner.String())
	}
	if _, err := instance.ReadRootIdentity(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview adopted identity: %v", err)
	}
}

func TestManualRootBannerIdentifiesTargetAndFailsClosed(t *testing.T) {
	root := initScheduledDemo(t)
	layout := instance.NewLayout(root)
	id, err := instance.ReadRootIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	var banner bytes.Buffer
	if err := prepareManualRoot(layout, &banner); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(banner.String(), id) || !strings.Contains(banner.String(), canonicalStatusRoot(root)) {
		t.Fatalf("wrong target: %s", banner.String())
	}
	if err := prepareManualRoot(layout, brokenRootBannerWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("failed display did not stop command: %v", err)
	}
	if _, err := instance.DecommissionRoot(context.Background(), root, "migrated", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := prepareManualRoot(layout, &banner); !errors.Is(err, instance.ErrHistoricalRoot) {
		t.Fatalf("historical mutation allowed: %v", err)
	}
	code, _, stderr := runArgs(t, "run", "example/scheduled", root)
	if code != 2 || !strings.Contains(stderr, "historical root; do not use") {
		t.Fatalf("run bypassed guard: %d %s", code, stderr)
	}
	for _, command := range []string{"abort", "cancel"} {
		code, _, stderr := runArgs(t, "run", command, "missing-run", root)
		if code != 2 || !strings.Contains(stderr, "historical root; do not use") {
			t.Fatalf("%s bypassed guard: %d %s", command, code, stderr)
		}
	}
	for _, args := range [][]string{
		{"approve", "--actor=test", "missing", "gate", root},
		{"override", "--actor=test", "--rationale=test", "missing", "gate", root},
		{"rerun-stage", "--actor=test", "--addendum=test", "missing", "gate", root},
		{"reset-rate-limit", root},
	} {
		code, _, stderr := runArgs(t, args...)
		if code != 2 || !strings.Contains(stderr, "historical root; do not use") {
			t.Fatalf("%v bypassed guard: %d %s", args, code, stderr)
		}
	}
}

func TestAuthoringCommandsRefuseHistoricalRoot(t *testing.T) {
	root, workflowPath := initFixTestInstance(t)
	before, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.DecommissionRoot(context.Background(), root, "migrated", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"signal", "test", root},
		{"engine-start", "default-implement", root},
		{"engine-project", "--gaggle=example", "run-1", root},
		{"self-update", root},
		{"fix", "--to=2.0", "--write", root},
		{"connect", "acme/web", "--token-env=GOOBERS_UNUSED_AUTHORING_TEST_TOKEN", root},
		{"scaffold", "goober", "new-goober", root},
		{"scaffold", "workflow", "new-workflow", root},
		{"scaffold", "gaggle", "new-gaggle", root},
		{"scaffold", "gaggle", "renamed", "--from=example", root},
	} {
		code, _, stderr := runArgs(t, args...)
		if code != 2 || !strings.Contains(stderr, "historical root; do not use") {
			t.Fatalf("%v bypassed historical guard: %d %s", args, code, stderr)
		}
	}
	after, err := os.ReadFile(workflowPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("historical workflow changed: %v", err)
	}
	code, _, stderr := runArgs(t, "fix", "--to=2.0", root)
	if code != 0 {
		t.Fatalf("read-only migration preview refused: %d %s", code, stderr)
	}
}
