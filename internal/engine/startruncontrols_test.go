package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runcontrol"
)

// TestRunInputRunControlsReachRunYAML verifies that the workflow persists
// supplied run controls in the journal identity.
func TestRunInputRunControlsReachRunYAML(t *testing.T) {

	// Deliberately not the defaults (45m / 3), and not derivable from them:
	// the worker receives the already-resolved run policy in RunInput.
	resolved := apiv1.RunControls{MaxRepasses: 7, StalledRunTimeout: "90m", MaxRunDuration: "8h"}
	in := projectionInput("pinned-controls", crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil))
	in.RunControls = resolved

	proj := executeForProjection(t, in, &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)

	dir, err := writeTestJournal(t, filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("write live journal: %v", err)
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("open projected journal: %v", err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatalf("read run.yaml identity: %v", err)
	}
	if identity.RunControls == nil {
		t.Fatal("run.yaml pinned no runControls block at all")
	}
	if got := identity.RunControls.StalledRunTimeout; got != "1h30m0s" {
		t.Errorf("run.yaml stalledRunTimeout = %q, want 1h30m0s; %q means the watchdog would enforce the built-in default against a run whose starter configured otherwise",
			got, runcontrol.DefaultStalledRunTimeout.String())
	}
	if got := identity.RunControls.MaxRepasses; got != 7 {
		t.Errorf("run.yaml maxRepasses = %d, want 7", got)
	}
	if got := identity.RunControls.MaxRunDuration; got != "8h0m0s" {
		t.Errorf("run.yaml maxRunDuration = %q, want 8h0m0s", got)
	}

	// The watchdog reads the serialized file, not the in-memory struct, so
	// assert the bytes on disk carry the value too.
	raw, err := os.ReadFile(filepath.Join(dir, "run.yaml"))
	if err != nil {
		t.Fatalf("read run.yaml: %v", err)
	}
	if !strings.Contains(string(raw), "stalledRunTimeout: 1h30m0s") {
		t.Errorf("run.yaml on disk does not carry stalledRunTimeout: 1h30m0s:\n%s", raw)
	}
}

// TestRunInputZeroRunControlsStillPinsDefaults keeps the legacy arm honest: a
// starter with no configuration (every existing test fixture, and any caller
// predating #3820) must still pin a complete, valid policy rather than an
// empty block — runcontrol.ValidatePinned rejects a partial one and the
// watchdog would skip the run.
func TestRunInputZeroRunControlsStillPinsDefaults(t *testing.T) {
	in := projectionInput("default-controls", crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil))
	proj := executeForProjection(t, in, &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)
	if proj.Identity.RunControls == nil {
		t.Fatal("projection pinned no runControls block")
	}
	if err := runcontrol.ValidatePinned(proj.Identity.RunControls); err != nil {
		t.Fatalf("pinned controls are not a complete policy: %v", err)
	}
	if got := proj.Identity.RunControls.StalledRunTimeout; got != runcontrol.DefaultStalledRunTimeout.String() {
		t.Errorf("unconfigured starter pinned stalledRunTimeout = %q, want the %s default", got, runcontrol.DefaultStalledRunTimeout)
	}
	if got := proj.Identity.RunControls.MaxRepasses; got != int32(runcontrol.DefaultMaxRepasses) {
		t.Errorf("unconfigured starter pinned maxRepasses = %d, want the %d default", got, runcontrol.DefaultMaxRepasses)
	}
}
