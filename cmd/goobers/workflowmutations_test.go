package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

// TestSetNonManualTriggersEnabledPreservesCommentsAndOrder makes sure the
// YAML mutator only touches the trigger it needs to and leaves every other
// line — comments, key order, indentation — bit-for-bit intact.
func TestSetNonManualTriggersEnabledPreservesCommentsAndOrder(t *testing.T) {
	src := []byte(`# top-of-file docs
apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  # human-friendly name
  name: default-implement
spec:
  # Trigger list drives all scheduling.
  triggers:
    - type: schedule
      # cron every hour
      cron: "0 * * * *"
    - type: manual
      # stays untouched
      note: "manual"
`)
	got, changed, err := setNonManualTriggersEnabled(src, true)
	if err != nil {
		t.Fatalf("setNonManualTriggersEnabled err = %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true when adding enabled")
	}
	out := string(got)
	// Comments/order preserved on unmodified lines.
	for _, marker := range []string{
		"# top-of-file docs",
		"# human-friendly name",
		"# Trigger list drives all scheduling.",
		"# cron every hour",
		"# stays untouched",
		`cron: "0 * * * *"`,
	} {
		if !strings.Contains(out, marker) {
			t.Errorf("missing preserved marker %q in output:\n%s", marker, out)
		}
	}
	// Schedule trigger has enabled: true appended after its existing keys.
	scheduleIdx := strings.Index(out, "type: schedule")
	manualIdx := strings.Index(out, "type: manual")
	if scheduleIdx < 0 || manualIdx < 0 {
		t.Fatalf("expected both triggers in output:\n%s", out)
	}
	scheduleBlock := out[scheduleIdx:manualIdx]
	if !strings.Contains(scheduleBlock, "enabled: true") {
		t.Errorf("schedule trigger did not gain enabled: true:\n%s", scheduleBlock)
	}
	// Manual trigger is not annotated.
	manualBlock := out[manualIdx:]
	if strings.Contains(manualBlock, "enabled:") {
		t.Errorf("manual trigger was rewritten but must be skipped:\n%s", manualBlock)
	}
	// Two-space indent under `triggers:` is preserved.
	if !strings.Contains(out, "\n    - type: schedule") {
		t.Errorf("expected 2-space nested indent for triggers, got:\n%s", out)
	}
}

// TestSetNonManualTriggersEnabledOverwrite covers switching an existing
// `enabled` value from true→false. The scalar node value is rewritten in
// place; the file still round-trips.
func TestSetNonManualTriggersEnabledOverwrite(t *testing.T) {
	src := []byte(`apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: sample
spec:
  triggers:
    - type: schedule
      enabled: true
      cron: "0 * * * *"
`)
	got, changed, err := setNonManualTriggersEnabled(src, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed = false, want true when flipping enabled")
	}
	if !strings.Contains(string(got), "enabled: false") {
		t.Errorf("expected enabled: false, got:\n%s", got)
	}
	if bytes.Contains(got, []byte("enabled: true")) {
		t.Errorf("prior enabled: true should have been overwritten:\n%s", got)
	}
}

// TestSetNonManualTriggersEnabledNoOp asserts the byte-verbatim contract for
// the unchanged path: when every non-manual trigger already has the desired
// value, the mutator returns the input bytes exactly and reports changed=false.
func TestSetNonManualTriggersEnabledNoOp(t *testing.T) {
	src := []byte(`apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: sample
spec:
  triggers:
    - type: schedule
      enabled: true
      cron: "0 * * * *"
    - type: manual
`)
	got, changed, err := setNonManualTriggersEnabled(src, true)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("changed = true, want false when everything already matches")
	}
	if &got[0] != &src[0] {
		// Same underlying array — the mutator returns raw unchanged, and both
		// side effects (no re-marshal, no reformat) matter, so compare bytes.
		if !bytes.Equal(got, src) {
			t.Errorf("no-op path returned different bytes than input")
		}
	}
}

// TestSetNonManualTriggersEnabledSkipsUntypedTriggers covers the guard for a
// trigger entry that omits a `type` key entirely. daemon.go treats absent
// type as "not schedulable", so setNonManualTriggersEnabled must not annotate
// it either.
func TestSetNonManualTriggersEnabledSkipsUntypedTriggers(t *testing.T) {
	src := []byte(`apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: sample
spec:
  triggers:
    - name: legacy-entry
    - type: schedule
      cron: "0 * * * *"
`)
	got, changed, err := setNonManualTriggersEnabled(src, true)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected the typed schedule trigger to be annotated")
	}
	out := string(got)
	// The un-typed entry keeps only the fields it had.
	untyped := out[strings.Index(out, "- name: legacy-entry"):strings.Index(out, "- type: schedule")]
	if strings.Contains(untyped, "enabled:") {
		t.Errorf("un-typed trigger must not gain enabled key, got block:\n%s", untyped)
	}
	// The typed one was annotated.
	if !strings.Contains(out[strings.Index(out, "- type: schedule"):], "enabled: true") {
		t.Errorf("typed schedule trigger should have gained enabled: true, got:\n%s", out)
	}
}

// TestSetNonManualTriggersEnabledErrors covers the three explicit error
// branches: missing spec, missing spec.triggers, and unparseable YAML. Each
// returns a wrapped error and leaves changed=false.
func TestSetNonManualTriggersEnabledErrors(t *testing.T) {
	tests := []struct {
		name         string
		src          string
		wantContains string
	}{
		{
			name:         "unparseable YAML",
			src:          "not: [valid\n",
			wantContains: "parse workflow YAML",
		},
		{
			name:         "missing spec",
			src:          "apiVersion: v1\nkind: Workflow\n",
			wantContains: "no spec",
		},
		{
			name: "missing spec.triggers",
			src: `apiVersion: v1
kind: Workflow
spec:
  other: value
`,
			wantContains: "spec.triggers",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, changed, err := setNonManualTriggersEnabled([]byte(test.src), true)
			if err == nil {
				t.Fatalf("err = nil, want an error containing %q", test.wantContains)
			}
			if !strings.Contains(err.Error(), test.wantContains) {
				t.Errorf("err = %v, want to contain %q", err, test.wantContains)
			}
			if changed {
				t.Errorf("changed = true on error path")
			}
			if got != nil {
				t.Errorf("bytes = %d, want nil on error", len(got))
			}
		})
	}
}

// TestSetWorkflowEnabledUnavailableWithoutReloader verifies the nil-reloader
// short-circuit: the service is safe to construct and expose before
// AttachReloader has been called, and returns a 503 intervention error until
// then.
func TestSetWorkflowEnabledUnavailableWithoutReloader(t *testing.T) {
	svc := newWorkflowMutationService(instance.Layout{})
	_, err := svc.SetWorkflowEnabled(context.Background(), httpapi.WorkflowEnabledRequest{
		Gaggle:   "web",
		Workflow: "implement",
		Enabled:  true,
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) {
		t.Fatalf("err = %v, want InterventionError", err)
	}
	if interventionErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", interventionErr.Status)
	}
	if interventionErr.Code != "workflow_mutations_unavailable" {
		t.Errorf("code = %q, want workflow_mutations_unavailable", interventionErr.Code)
	}
}

// fakeConfigReloadHandle is a test double for configReloadHandle that lets
// each SetWorkflowEnabled case drive workflowSource and pollOnce directly
// without the full daemon reload machinery poll() transitively touches.
type fakeConfigReloadHandle struct {
	sources     map[[2]string]string
	pollCalls   int
	pollNow     time.Time
	pollApplied bool
	pollOld     string
	pollNew     string
	pollReject  string
	pollErr     error
	// pollOnceHook, when set, runs on every pollOnce call before returning
	// the fields above. Tests use it to snapshot the on-disk file contents
	// the reload would have observed.
	pollOnceHook func(now time.Time)
}

func (f *fakeConfigReloadHandle) workflowSource(gaggle, workflow string) (string, bool) {
	source, ok := f.sources[[2]string{gaggle, workflow}]
	return source, ok
}

func (f *fakeConfigReloadHandle) pollOnce(now time.Time) (bool, string, string, string, error) {
	f.pollCalls++
	f.pollNow = now
	if f.pollOnceHook != nil {
		f.pollOnceHook(now)
	}
	return f.pollApplied, f.pollOld, f.pollNew, f.pollReject, f.pollErr
}

// TestSetWorkflowEnabledRejectsBlankIdentifiers covers the input-validation
// branch: even with a live reloader, an empty gaggle or workflow field must
// be rejected before any config lookup.
func TestSetWorkflowEnabledRejectsBlankIdentifiers(t *testing.T) {
	svc := newWorkflowMutationService(instance.Layout{})
	// Attach a non-nil handle so we pass the availability gate and reach the
	// identifier check. The handle is never dereferenced for this test.
	svc.AttachReloader(&fakeConfigReloadHandle{})

	tests := []struct {
		name  string
		input httpapi.WorkflowEnabledRequest
	}{
		{name: "empty gaggle", input: httpapi.WorkflowEnabledRequest{Workflow: "implement", Enabled: true}},
		{name: "empty workflow", input: httpapi.WorkflowEnabledRequest{Gaggle: "web", Enabled: true}},
		{name: "whitespace only", input: httpapi.WorkflowEnabledRequest{Gaggle: "  ", Workflow: "\t", Enabled: false}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := svc.SetWorkflowEnabled(context.Background(), test.input)
			var interventionErr *httpapi.InterventionError
			if !errors.As(err, &interventionErr) {
				t.Fatalf("err = %v, want InterventionError", err)
			}
			if interventionErr.Status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", interventionErr.Status)
			}
			if interventionErr.Code != "invalid_request" {
				t.Errorf("code = %q, want invalid_request", interventionErr.Code)
			}
		})
	}
}

// workflowMutationFixture returns a service pointed at a config directory
// under t.TempDir() containing a single workflow YAML at the given source
// path, and a fake handle whose workflowSource resolves that gaggle/workflow
// to that source. The service is otherwise fully wired to exercise the real
// on-disk read/edit/atomic-write/rollback path.
func workflowMutationFixture(t *testing.T, gaggle, workflow, source, yaml string) (*workflowMutationService, *fakeConfigReloadHandle, string) {
	t.Helper()
	root := t.TempDir()
	layout := instance.NewLayout(root)
	path := filepath.Join(layout.ConfigDir(), source)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := newWorkflowMutationService(layout)
	handle := &fakeConfigReloadHandle{
		sources: map[[2]string]string{{gaggle, workflow}: source},
	}
	svc.AttachReloader(handle)
	return svc, handle, path
}

const disableTriggerWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: implement
spec:
  gaggle: web
  triggers:
    - type: schedule
      # cron once an hour
      cron: "0 * * * *"
      enabled: true
    - type: manual
      # left untouched
      note: "manual"
`

// TestSetWorkflowEnabledReturnsNotFoundForUnknownWorkflow verifies the
// WorkflowSource miss branch: when the applied definitions do not contain
// gaggle/workflow, the service must fail closed with a 404 intervention
// error and MUST NOT touch the on-disk config or drive a reload.
func TestSetWorkflowEnabledReturnsNotFoundForUnknownWorkflow(t *testing.T) {
	source := filepath.ToSlash(filepath.Join("gaggles", "web", "workflows", "implement.yaml"))
	svc, handle, path := workflowMutationFixture(t, "web", "implement", source, disableTriggerWorkflowYAML)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.SetWorkflowEnabled(context.Background(), httpapi.WorkflowEnabledRequest{
		Gaggle:   "web",
		Workflow: "does-not-exist",
		Enabled:  false,
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) {
		t.Fatalf("err = %v, want InterventionError", err)
	}
	if interventionErr.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", interventionErr.Status)
	}
	if interventionErr.Code != "workflow_not_found" {
		t.Errorf("code = %q, want workflow_not_found", interventionErr.Code)
	}
	if handle.pollCalls != 0 {
		t.Errorf("pollOnce calls = %d, want zero on a 404", handle.pollCalls)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("workflow YAML was rewritten on 404: before=%q after=%q", before, after)
	}
}

// TestSetWorkflowEnabledRestoresFileOnReloadRejection covers the write +
// reload contract on the rejection branch: the file IS rewritten before the
// reload is driven (so the poll sees the new bytes), and when pollOnce
// reports a rejection the service MUST restore the original bytes so a
// subsequent apply/reload does not keep re-observing the same bad edit and
// the atomicity the caller advertised is preserved.
func TestSetWorkflowEnabledRestoresFileOnReloadRejection(t *testing.T) {
	source := filepath.ToSlash(filepath.Join("gaggles", "web", "workflows", "implement.yaml"))
	svc, handle, path := workflowMutationFixture(t, "web", "implement", source, disableTriggerWorkflowYAML)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Sanity-check the invariant that makes this test meaningful: the file
	// must be on disk with the *edited* bytes at the moment pollOnce runs.
	// If the service handed the reloader the pre-edit bytes, the "restore"
	// contract would be trivially satisfied by never having written.
	var duringPoll []byte
	handle.pollReject = "webhook trigger topology changed"
	handle.pollOnceHook = func(time.Time) {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Errorf("read workflow during pollOnce: %v", readErr)
			return
		}
		duringPoll = content
	}

	_, err = svc.SetWorkflowEnabled(context.Background(), httpapi.WorkflowEnabledRequest{
		Gaggle:   "web",
		Workflow: "implement",
		Enabled:  false,
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) {
		t.Fatalf("err = %v, want InterventionError", err)
	}
	if interventionErr.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", interventionErr.Status)
	}
	if interventionErr.Code != "workflow_edit_rejected" {
		t.Errorf("code = %q, want workflow_edit_rejected", interventionErr.Code)
	}
	if interventionErr.Message != "webhook trigger topology changed" {
		t.Errorf("message = %q, want the reloader's rejection reason to surface", interventionErr.Message)
	}
	if handle.pollCalls != 1 {
		t.Fatalf("pollOnce calls = %d, want one", handle.pollCalls)
	}

	if duringPoll == nil {
		t.Fatal("pollOnce hook did not run; the reload wasn't driven at all")
	}
	if bytes.Equal(duringPoll, before) {
		t.Errorf("pollOnce saw the pre-edit file; the service must write BEFORE reloading:\n%s", duringPoll)
	}
	if !strings.Contains(string(duringPoll), "enabled: false") {
		t.Errorf("pollOnce did not see the edited enabled: false, got:\n%s", duringPoll)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("workflow YAML was not restored to the pre-edit bytes after a 422 rejection:\nwant:\n%s\ngot:\n%s", before, after)
	}
}

// TestSetWorkflowEnabledAppliesEditAndReturnsResult exercises the happy path:
// non-manual triggers are toggled in the on-disk file, the manual trigger is
// left alone, pollOnce is driven exactly once against the edited bytes, and
// the response echoes gaggle/workflow/enabled.
func TestSetWorkflowEnabledAppliesEditAndReturnsResult(t *testing.T) {
	source := filepath.ToSlash(filepath.Join("gaggles", "web", "workflows", "implement.yaml"))
	svc, handle, path := workflowMutationFixture(t, "web", "implement", source, disableTriggerWorkflowYAML)
	handle.pollApplied = true
	handle.pollOld = "old-digest"
	handle.pollNew = "new-digest"
	var seenByReload []byte
	handle.pollOnceHook = func(time.Time) {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Errorf("read workflow during pollOnce: %v", readErr)
			return
		}
		seenByReload = content
	}

	result, err := svc.SetWorkflowEnabled(context.Background(), httpapi.WorkflowEnabledRequest{
		Gaggle:   "web",
		Workflow: "implement",
		Enabled:  false,
	})
	if err != nil {
		t.Fatalf("SetWorkflowEnabled err = %v", err)
	}
	want := httpapi.WorkflowEnabledResult{Gaggle: "web", Workflow: "implement", Enabled: false}
	if result != want {
		t.Errorf("result = %+v, want %+v", result, want)
	}
	if handle.pollCalls != 1 {
		t.Fatalf("pollOnce calls = %d, want one", handle.pollCalls)
	}

	if seenByReload == nil {
		t.Fatal("pollOnce hook did not run; reload was never driven")
	}
	// The reload observed the edited file: schedule got enabled: false, and
	// the manual trigger below stayed as it was.
	scheduleIdx := bytes.Index(seenByReload, []byte("type: schedule"))
	manualIdx := bytes.Index(seenByReload, []byte("type: manual"))
	if scheduleIdx < 0 || manualIdx < 0 {
		t.Fatalf("expected both triggers in reload-observed file:\n%s", seenByReload)
	}
	scheduleBlock := seenByReload[scheduleIdx:manualIdx]
	if !bytes.Contains(scheduleBlock, []byte("enabled: false")) {
		t.Errorf("schedule trigger did not get enabled: false, got block:\n%s", scheduleBlock)
	}
	if bytes.Contains(scheduleBlock, []byte("enabled: true")) {
		t.Errorf("prior enabled: true was not overwritten on schedule block:\n%s", scheduleBlock)
	}
	manualBlock := seenByReload[manualIdx:]
	if bytes.Contains(manualBlock, []byte("enabled:")) {
		t.Errorf("manual trigger gained an enabled: field, but must be left untouched:\n%s", manualBlock)
	}
	if !bytes.Contains(seenByReload, []byte(`# cron once an hour`)) || !bytes.Contains(seenByReload, []byte(`# left untouched`)) {
		t.Errorf("preserved comments were dropped on the reload-observed file:\n%s", seenByReload)
	}

	// The file on disk after success is the edited bytes — no rollback.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, seenByReload) {
		t.Errorf("post-success file diverged from the reload-observed bytes:\nwant:\n%s\ngot:\n%s", seenByReload, after)
	}
}
