package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// Selective parallel conformance corpus (design plan-driven-dynamic-fan-out.md
// §14 cases 1–8, 21 and 22). Every fixture runs the §5.1 shape: a triage task
// publishes a selection document into its `lenses` slot, and a parallel with
// one always-run baseline branch and three optional lenses reads it.

const selectiveRunPrefix = "selective"

type selectiveOptions struct {
	width       int32
	policy      apiv1.BranchFailurePolicy
	onFailure   string
	allOptional bool
}

// selectiveMachine compiles the §5.1 example: triage → review{baseline,
// security?, migration?, ui?} → collate. Branch ids are therefore
// baseline=1, security=2, migration=3, ui=4.
func selectiveMachine(t *testing.T, opts selectiveOptions) *workflow.Machine {
	t.Helper()
	if opts.width == 0 {
		opts.width = 1
	}
	if opts.policy == "" {
		opts.policy = apiv1.BranchContinueOnError
	}
	task := func(name, next string) apiv1.Task {
		return apiv1.Task{Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: next}
	}
	triage := task("triage", "review")
	triage.Goal = "Decide which review lenses apply to this change."
	triage.ArtifactSlots = []apiv1.ArtifactSlot{{Name: "lenses", MediaType: "application/json"}}
	parallel := apiv1.Parallel{
		Name: "review", FailurePolicy: opts.policy, OnFailure: opts.onFailure,
		BranchTimeoutSeconds: 1800, MaxConcurrentBranches: opts.width, Join: "collate",
		Selection: &apiv1.ParallelSelection{Source: apiv1.FanOutSource{Task: "triage", Slot: "lenses"}},
		Branches: []apiv1.Branch{
			{Name: "baseline", Start: "review-baseline", Optional: opts.allOptional},
			{Name: "security", Start: "review-security", Optional: true},
			{Name: "migration", Start: "review-migration", Optional: true},
			{Name: "ui", Start: "review-ui", Optional: true},
		},
	}
	tasks := []apiv1.Task{
		triage,
		task("review-baseline", workflow.TargetJoin), task("review-security", workflow.TargetJoin),
		task("review-migration", workflow.TargetJoin), task("review-ui", workflow.TargetJoin),
		task("collate", workflow.TerminalComplete),
	}
	if opts.onFailure == "triage-failed" {
		tasks = append(tasks, task("triage-failed", workflow.TerminalComplete))
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "selective-review", Version: 1, DSLVersion: "3.1",
		Annotations: map[string]string{"goobers.dev/allow-preview-features": "true"},
		Spec: apiv1.WorkflowSpec{
			Gaggle: "demo", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "triage",
			Tasks:     tasks,
			Parallels: []apiv1.Parallel{parallel},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile selective fixture: %v", err)
	}
	return machine
}

// selectionDeterministic publishes the triage selection through the real
// named artifact-set boundary (Prepare → Bind → Publish), exactly as the shell
// executor does, and scripts every other stage.
type selectionDeterministic struct {
	rec      ArtifactRecorder
	document []byte
	results  map[string]stubTaskResult
	mu       *sync.Mutex
	calls    map[string]int
}

func (d *selectionDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	stage := env.TaskID[strings.LastIndex(env.TaskID, ":")+1:]
	d.mu.Lock()
	d.calls[stage]++
	d.mu.Unlock()
	if stage != "triage" {
		cfg, ok := d.results[stage]
		if !ok {
			cfg = stubTaskResult{status: apiv1.ResultSuccess, outputs: map[string]any{"ran": stage}}
		}
		return apiv1.ResultEnvelope{Status: cfg.status, Outputs: cfg.outputs, Error: cfg.errorInfo}, nil
	}
	if env.ArtifactPublication == nil {
		return apiv1.ResultEnvelope{}, errors.New("triage dispatched without its slot publication contract")
	}
	dir, err := os.MkdirTemp("", "selection-*")
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "lenses.json"), d.document, 0o600); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	manifest, _ := json.Marshal(artifactset.Manifest{SchemaVersion: artifactset.SchemaVersion,
		Entries: []artifactset.ManifestEntry{{Name: "lenses", Path: "lenses.json", MediaType: "application/json"}}})
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o600); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	_, scrubber := journal.DefaultScrubber()
	prepared, err := artifactset.Prepare(ctx, dir, "manifest.json", artifactset.NewSanitizer(scrubber))
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if err := prepared.Bind(env.ArtifactPublication, env.Attempt); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	pointers, err := prepared.Publish(ctx, func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
		ref, err := d.rec.RecordArtifact(env.TaskID+"/"+name, data)
		return apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: media, Integrity: ref.Integrity}, err
	})
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Artifacts: pointers}, nil
}

type selectiveHarness struct {
	runner  *Runner
	runsDir string
	mu      sync.Mutex
	calls   map[string]int
}

func newSelectiveHarness(t *testing.T, document string, results map[string]stubTaskResult) *selectiveHarness {
	t.Helper()
	h := &selectiveHarness{calls: map[string]int{}}
	h.runner, h.runsDir = newParallelTestRunner(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &selectionDeterministic{
			rec: rec, document: []byte(document), results: results, mu: &h.mu, calls: h.calls,
		}, nil
	})
	return h
}

func (h *selectiveHarness) start(t *testing.T, runID string, machine *workflow.Machine) Result {
	t.Helper()
	result, err := h.runner.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo", Machine: machine, Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return result
}

func (h *selectiveHarness) callCount(stage string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[stage]
}

func (h *selectiveHarness) events(t *testing.T, runID string) []journal.Event {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(h.runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func eventsOfType(events []journal.Event, kind journal.EventType) []journal.Event {
	var out []journal.Event
	for _, event := range events {
		if event.Type == kind {
			out = append(out, event)
		}
	}
	return out
}

func startedBranches(events []journal.Event) []string {
	var names []string
	for _, event := range eventsOfType(events, journal.EventBranchStarted) {
		names = append(names, fmt.Sprintf("%d:%s", event.Branch, event.BranchName))
	}
	return names
}

func completenessSummary(record []journal.BranchOutcome) string {
	parts := make([]string, 0, len(record))
	for _, entry := range record {
		parts = append(parts, fmt.Sprintf("%d:%s:%s", entry.Branch, entry.Name, entry.Status))
	}
	return strings.Join(parts, ",")
}

func joinRuns(events []journal.Event) int {
	count := 0
	for _, event := range eventsOfType(events, journal.EventStageStarted) {
		if event.Stage == "collate" {
			count++
		}
	}
	return count
}

func widths() []int32 { return []int32{1, 3} }

// §14 case 1 (and the §5.1 local-runner example): a subset selection runs the
// baseline branch and exactly the selected optional branch.
func TestConformanceSelectiveParallelRunsSelectedSubset(t *testing.T) {
	for _, width := range widths() {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			h := newSelectiveHarness(t, `{"schemaVersion":1,"branches":[{"name":"security","reason":"touches internal/auth"}]}`, nil)
			runID := fmt.Sprintf("%s-subset-%d", selectiveRunPrefix, width)
			result := h.start(t, runID, selectiveMachine(t, selectiveOptions{width: width}))
			if result.Phase != journal.PhaseCompleted {
				t.Fatalf("phase = %q, want completed", result.Phase)
			}
			events := h.events(t, runID)
			if got := strings.Join(startedBranches(events), ","); got != "1:baseline,2:security" {
				t.Fatalf("started branches = %s, want baseline and security only", got)
			}
			for stage, want := range map[string]int{"triage": 1, "review-baseline": 1, "review-security": 1, "review-migration": 0, "review-ui": 0, "collate": 1} {
				if got := h.callCount(stage); got != want {
					t.Errorf("%s ran %d times, want %d", stage, got, want)
				}
			}
			started := eventsOfType(events, journal.EventParallelStarted)
			if len(started) != 1 || started[0].Selection == nil {
				t.Fatalf("parallel.started = %+v, want one with a frozen selection", started)
			}
			selection := started[0].Selection
			if strings.Join(selection.Selected, ",") != "security" || selection.Reasons["security"] != "touches internal/auth" ||
				selection.Source.Task != "triage" || selection.Source.Slot != "lenses" || selection.Source.Attempt != 1 ||
				!strings.HasPrefix(selection.SourceDigest, "sha256:") || selection.Integrity != apiv1.IntegrityDerived {
				t.Fatalf("selection = %+v", selection)
			}
			finished := parallelCompleteness(t, h.runsDir, runID)
			if got := completenessSummary(finished); got != "1:baseline:succeeded,2:security:succeeded,3:migration:skipped,4:ui:skipped" {
				t.Fatalf("completeness = %s", got)
			}
			if finished[1].Reason != "touches internal/auth" || finished[2].Reason != "not selected by triage.lenses" || finished[0].Reason != "" {
				t.Fatalf("completeness reasons = %+v", finished)
			}
			if finished[2].Artifacts != 0 || finished[3].Artifacts != 0 {
				t.Fatalf("skipped branches report artifacts: %+v", finished)
			}
		})
	}
}

// §14 case 2: an empty selection runs only the non-optional branch and the
// join exactly once.
func TestConformanceSelectiveParallelEmptySelectionRunsBaselineOnly(t *testing.T) {
	for _, width := range widths() {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			h := newSelectiveHarness(t, `{"schemaVersion":1,"branches":[]}`, nil)
			runID := fmt.Sprintf("%s-empty-%d", selectiveRunPrefix, width)
			if result := h.start(t, runID, selectiveMachine(t, selectiveOptions{width: width})); result.Phase != journal.PhaseCompleted {
				t.Fatalf("phase = %q", result.Phase)
			}
			events := h.events(t, runID)
			if got := strings.Join(startedBranches(events), ","); got != "1:baseline" {
				t.Fatalf("started branches = %s", got)
			}
			if got := joinRuns(events); got != 1 {
				t.Fatalf("join ran %d times, want 1", got)
			}
			if got := completenessSummary(parallelCompleteness(t, h.runsDir, runID)); got != "1:baseline:succeeded,2:security:skipped,3:migration:skipped,4:ui:skipped" {
				t.Fatalf("completeness = %s", got)
			}
		})
	}
}

// §14 case 3: every branch optional and none selected — no branch starts, the
// join still runs exactly once, and every completeness entry is skipped.
func TestConformanceSelectiveParallelAllSkippedStillJoinsOnce(t *testing.T) {
	for _, width := range widths() {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			h := newSelectiveHarness(t, `{"schemaVersion":1,"branches":[]}`, nil)
			runID := fmt.Sprintf("%s-none-%d", selectiveRunPrefix, width)
			if result := h.start(t, runID, selectiveMachine(t, selectiveOptions{width: width, allOptional: true})); result.Phase != journal.PhaseCompleted {
				t.Fatalf("phase = %q", result.Phase)
			}
			events := h.events(t, runID)
			if got := startedBranches(events); len(got) != 0 {
				t.Fatalf("branches started = %v, want none", got)
			}
			if got := eventsOfType(events, journal.EventBranchFinished); len(got) != 0 {
				t.Fatalf("branch.finished events = %+v, want none", got)
			}
			if got := joinRuns(events); got != 1 {
				t.Fatalf("join ran %d times, want 1", got)
			}
			if got := completenessSummary(parallelCompleteness(t, h.runsDir, runID)); got != "1:baseline:skipped,2:security:skipped,3:migration:skipped,4:ui:skipped" {
				t.Fatalf("completeness = %s", got)
			}
		})
	}
}

// §14 case 4 and case 21 (unrepaired document): unknown names, duplicate
// names, a document that violates the envelope, and a document above the host
// bound all fail with fanout_source_invalid BEFORE parallel.started. With no
// onFailure the parallel takes @abort; a declared onFailure is followed.
func TestConformanceSelectiveParallelInvalidSourceFailsBeforeStart(t *testing.T) {
	oversized := `{"schemaVersion":1,"branches":[{"name":"security","reason":"` + strings.Repeat("x", int(workflow.MaxFanOutSourceBytes)) + `"}]}`
	for _, tc := range []struct {
		name, document, want string
	}{
		{"unknown name", `{"schemaVersion":1,"branches":[{"name":"performance"}]}`, `unknown branch "performance"`},
		{"duplicate name", `{"schemaVersion":1,"branches":[{"name":"ui"},{"name":"ui"}]}`, `branch "ui" more than once`},
		{"envelope violation", `{"schemaVersion":1,"branches":["security"]}`, "violates fanout-selection.schema.json"},
		{"missing schema version", `{"branches":[]}`, "violates fanout-selection.schema.json"},
		{"above host bound", oversized, "exceeds byte limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSelectiveHarness(t, tc.document, nil)
			runID := selectiveRunPrefix + "-invalid"
			if result := h.start(t, runID, selectiveMachine(t, selectiveOptions{})); result.Phase != journal.PhaseAborted {
				t.Fatalf("phase = %q, want aborted (no onFailure declared)", result.Phase)
			}
			events := h.events(t, runID)
			if got := eventsOfType(events, journal.EventParallelStarted); len(got) != 0 {
				t.Fatalf("parallel.started written for an invalid source: %+v", got)
			}
			assertFanOutSourceInvalid(t, events, tc.want)
			for _, stage := range []string{"review-baseline", "review-security", "collate"} {
				if got := h.callCount(stage); got != 0 {
					t.Errorf("%s ran %d times after an invalid source", stage, got)
				}
			}
		})
	}
	t.Run("onFailure route", func(t *testing.T) {
		h := newSelectiveHarness(t, `{"schemaVersion":1,"branches":[{"name":"performance"}]}`, nil)
		runID := selectiveRunPrefix + "-invalid-routed"
		machine := selectiveMachine(t, selectiveOptions{policy: apiv1.BranchAllOrNothing, onFailure: "triage-failed"})
		if result := h.start(t, runID, machine); result.Phase != journal.PhaseCompleted {
			t.Fatalf("phase = %q, want completed through onFailure", result.Phase)
		}
		events := h.events(t, runID)
		assertFanOutSourceInvalid(t, events, "unknown branch")
		if h.callCount("triage-failed") != 1 || h.callCount("collate") != 0 || len(eventsOfType(events, journal.EventParallelStarted)) != 0 {
			t.Fatalf("onFailure not followed: triage-failed=%d collate=%d", h.callCount("triage-failed"), h.callCount("collate"))
		}
	})
	t.Run("escalate route", func(t *testing.T) {
		h := newSelectiveHarness(t, `{"schemaVersion":1,"branches":[{"name":"ui"},{"name":"ui"}]}`, nil)
		machine := selectiveMachine(t, selectiveOptions{policy: apiv1.BranchFailFast, onFailure: workflow.TargetEscalate})
		if result := h.start(t, selectiveRunPrefix+"-invalid-escalated", machine); result.Phase != journal.PhaseEscalated {
			t.Fatalf("phase = %q, want escalated", result.Phase)
		}
	})
}

func TestSelectiveParallelRefusalMessagesRemainExact(t *testing.T) {
	multiIssue := `{"branches":["one","two","three","four"]}`
	tests := []struct {
		name     string
		document string
		want     string
	}{
		{
			name: "unknown branch", document: `{"schemaVersion":1,"branches":[{"name":"performance"}]}`,
			want: `runner: fan-out "review" source triage.lenses is invalid: names unknown branch "performance"`,
		},
		{
			name: "duplicate branch", document: `{"schemaVersion":1,"branches":[{"name":"ui"},{"name":"ui"}]}`,
			want: `runner: fan-out "review" source triage.lenses is invalid: names branch "ui" more than once`,
		},
		{
			name: "reason bound",
			document: `{"schemaVersion":1,"branches":[{"name":"ui","reason":"` +
				strings.Repeat("é", fanout.MaxSelectionReasonBytes/2+1) + `"}]}`,
			want: `runner: fan-out "review" source triage.lenses is invalid: reason for branch "ui" exceeds 512 bytes`,
		},
		{
			name: "schema", document: `{"schemaVersion":1,"branches":["security"]}`,
			want: `runner: fan-out "review" source triage.lenses is invalid: document violates fanout-selection.schema.json: /branches/0: expected object, but got string`,
		},
		{
			name: "multi-issue schema", document: multiIssue,
			want: `runner: fan-out "review" source triage.lenses is invalid: document violates fanout-selection.schema.json: /: missing properties: 'schemaVersion'; /branches/0: expected object, but got string; /branches/1: expected object, but got string; and 2 more`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newSelectiveHarness(t, test.document, nil)
			runID := selectiveRunPrefix + "-exact-" + strings.ReplaceAll(test.name, " ", "-")
			h.start(t, runID, selectiveMachine(t, selectiveOptions{}))
			if got := fanOutSourceInvalidMessage(t, h.events(t, runID)); got != test.want {
				t.Fatalf("journaled Error.Message =\n%q\nwant\n%q", got, test.want)
			}
		})
	}

	for _, test := range []struct {
		name, detail, want string
	}{
		{
			name: "media type", detail: `media type "text/plain" is not application/json`,
			want: `runner: fan-out "review" source triage.lenses is invalid: media type "text/plain" is not application/json`,
		},
		{
			name: "decode", detail: "decode selection document: unexpected EOF",
			want: `runner: fan-out "review" source triage.lenses is invalid: decode selection document: unexpected EOF`,
		},
		{
			name: "integrity", detail: "source slot triage.lenses: artifact digest mismatch",
			want: `runner: fan-out "review" source triage.lenses is invalid: source slot triage.lenses: artifact digest mismatch`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cause := &FanOutSourceError{
				State: "review", Source: apiv1.FanOutSource{Task: "triage", Slot: "lenses"},
				Err: errors.New(test.detail),
			}
			got := journal.ErrorDetailFor(FanOutSourceInvalidCode, cause).Message
			if got != test.want {
				t.Fatalf("journaled Error.Message = %q, want %q", got, test.want)
			}
		})
	}
}

func fanOutSourceInvalidMessage(t *testing.T, events []journal.Event) string {
	t.Helper()
	for _, event := range eventsOfType(events, journal.EventError) {
		if event.Error != nil && event.Error.Code == FanOutSourceInvalidCode && event.Parallel == "review" {
			return event.Error.Message
		}
	}
	t.Fatalf("no fanout_source_invalid error event in %+v", eventsOfType(events, journal.EventError))
	return ""
}

func assertFanOutSourceInvalid(t *testing.T, events []journal.Event, want string) {
	t.Helper()
	for _, event := range eventsOfType(events, journal.EventError) {
		if event.Error != nil && event.Error.Code == FanOutSourceInvalidCode && event.Parallel == "review" {
			if !strings.Contains(event.Error.Message, want) {
				t.Fatalf("fanout_source_invalid message = %q, want it to mention %q", event.Error.Message, want)
			}
			return
		}
	}
	t.Fatalf("no fanout_source_invalid error event for parallel review in %+v", eventsOfType(events, journal.EventError))
}

// §14 case 5: skipped branches keep their declared ids and selected ids never
// shift; the (branch, name) pairs match a selection that runs everything.
func TestConformanceSelectiveParallelBranchIDsAreDeclarationOrdered(t *testing.T) {
	pairs := func(document string) (string, string) {
		h := newSelectiveHarness(t, document, nil)
		runID := selectiveRunPrefix + "-ids"
		h.start(t, runID, selectiveMachine(t, selectiveOptions{width: 3}))
		var ids []string
		for _, entry := range parallelCompleteness(t, h.runsDir, runID) {
			ids = append(ids, fmt.Sprintf("%d:%s", entry.Branch, entry.Name))
		}
		return strings.Join(ids, ","), strings.Join(startedBranches(h.events(t, runID)), ",")
	}
	all, allStarted := pairs(`{"schemaVersion":1,"branches":[{"name":"security"},{"name":"migration"},{"name":"ui"}]}`)
	some, someStarted := pairs(`{"schemaVersion":1,"branches":[{"name":"ui"}]}`)
	if all != some || all != "1:baseline,2:security,3:migration,4:ui" {
		t.Fatalf("completeness ids differ by selection: all=%s subset=%s", all, some)
	}
	if !strings.Contains(allStarted, "4:ui") || someStarted != "1:baseline,4:ui" && someStarted != "4:ui,1:baseline" {
		t.Fatalf("started branch ids shifted: all=%s subset=%s", allStarted, someStarted)
	}
}

// §14 case 6: fail_fast and all_or_nothing treat skipped branches as neither
// failed nor cancelled. fail_fast cancels only selected branches that had not
// started; all_or_nothing succeeds when every SELECTED branch succeeds.
func TestConformanceSelectiveParallelPoliciesIgnoreSkippedBranches(t *testing.T) {
	t.Run("fail_fast", func(t *testing.T) {
		failing := map[string]stubTaskResult{"review-baseline": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "lens_failed", Message: "lens failed"}}}
		h := newSelectiveHarness(t, `{"schemaVersion":1,"branches":[{"name":"ui"}]}`, failing)
		runID := selectiveRunPrefix + "-fail-fast"
		machine := selectiveMachine(t, selectiveOptions{policy: apiv1.BranchFailFast, onFailure: workflow.TargetAbort})
		if result := h.start(t, runID, machine); result.Phase != journal.PhaseAborted {
			t.Fatalf("phase = %q, want aborted", result.Phase)
		}
		if got := completenessSummary(parallelCompleteness(t, h.runsDir, runID)); got != "1:baseline:failed,2:security:skipped,3:migration:skipped,4:ui:cancelled" {
			t.Fatalf("completeness = %s", got)
		}
	})
	t.Run("all_or_nothing", func(t *testing.T) {
		for _, width := range widths() {
			h := newSelectiveHarness(t, `{"schemaVersion":1,"branches":[{"name":"security"}]}`, nil)
			runID := fmt.Sprintf("%s-all-or-nothing-%d", selectiveRunPrefix, width)
			machine := selectiveMachine(t, selectiveOptions{width: width, policy: apiv1.BranchAllOrNothing, onFailure: workflow.TargetAbort})
			if result := h.start(t, runID, machine); result.Phase != journal.PhaseCompleted {
				t.Fatalf("width %d phase = %q, want completed: skipped branches are not failures", width, result.Phase)
			}
			finished := eventsOfType(h.events(t, runID), journal.EventParallelFinished)
			if len(finished) != 1 || finished[0].Target != "collate" {
				t.Fatalf("width %d parallel.finished = %+v, want routed to the join", width, finished)
			}
		}
	})
	t.Run("continue_on_error with all skipped is not a failure", func(t *testing.T) {
		par := newParallelExec(selectiveMachine(t, selectiveOptions{allOptional: true}).Def.Spec.Parallels[0])
		par.applySelection(&journal.ParallelSelection{Source: journal.SelectionSource{Task: "triage", Slot: "lenses"}, Selected: []string{}})
		if par.anyFailed() {
			t.Fatal("skipped branches counted as failures")
		}
		if target, runJoin := par.route(); target != "collate" || !runJoin {
			t.Fatalf("route = %q, %v; want the join", target, runJoin)
		}
		if cancelled := par.cancelRemaining(); len(cancelled) != 0 {
			t.Fatalf("cancelRemaining cancelled skipped branches: %d", len(cancelled))
		}
	})
}

// §14 case 7's raw-journal half: the projection golden lives in
// TestConformanceStaticParallelProjectionUnchanged. A selective visit is the
// only one whose parallel.started projects a selection.
func TestConformanceSelectionProjectsOnlyOnSelectiveStart(t *testing.T) {
	h := newSelectiveHarness(t, `{"schemaVersion":1,"branches":[{"name":"security","reason":"auth"}]}`, nil)
	runID := selectiveRunPrefix + "-projection"
	h.start(t, runID, selectiveMachine(t, selectiveOptions{}))
	var withSelection []string
	for _, event := range journal.ConformanceView(h.events(t, runID)) {
		if event.Selection != "" {
			withSelection = append(withSelection, string(event.Type))
			if strings.Contains(event.String(), "auth") {
				t.Fatalf("reasons leaked into the conformance projection: %s", event.String())
			}
			if !strings.Contains(event.String(), "|selection=triage.lenses@1:sha256:") || !strings.HasSuffix(event.String(), ":[security]") {
				t.Fatalf("selection projection = %s", event.String())
			}
		}
	}
	if strings.Join(withSelection, ",") != string(journal.EventParallelStarted) {
		t.Fatalf("events projecting a selection = %v, want only parallel.started", withSelection)
	}
}

// selectiveJournal writes a journal that a selective run left behind when the
// runner stopped at a chosen boundary. The producer's recorded artifacts are
// deliberately unreadable: any attempt to re-resolve the source on resume
// would fail, so a completed resume proves the skip set came from the
// recorded parallel.started alone.
func selectiveJournal(t *testing.T, runsDir, runID string, machine *workflow.Machine, tail ...journal.Event) {
	t.Helper()
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "demo", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("review")
	missing := journal.Ref{Path: "artifacts/sha256-missing", Digest: "sha256:" + strings.Repeat("0", 64), Size: 10, MediaType: "application/json", Integrity: apiv1.IntegrityDerived}
	head := []journal.Event{
		{Type: journal.EventStageStarted, Stage: "triage", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "triage", Attempt: 1, Status: string(apiv1.ResultSuccess), Artifacts: []journal.Ref{missing, missing}},
		{
			Type: journal.EventParallelStarted, Parallel: "review",
			Selection: &journal.ParallelSelection{
				Source:       journal.SelectionSource{Task: "triage", Slot: "lenses", Visit: 2, Attempt: 1},
				SourceDigest: "sha256:" + strings.Repeat("a", 64), Integrity: apiv1.IntegrityDerived,
				Selected: []string{"ui"}, Reasons: map[string]string{"ui": "touches portal"},
			},
			Completeness: []journal.BranchOutcome{
				{Branch: 1, Name: "baseline", Status: journal.BranchCancelled},
				{Branch: 2, Name: "security", Status: journal.BranchSkipped, Reason: "not selected by triage.lenses"},
				{Branch: 3, Name: "migration", Status: journal.BranchSkipped, Reason: "not selected by triage.lenses"},
				{Branch: 4, Name: "ui", Status: journal.BranchCancelled, Reason: "touches portal"},
			},
		},
	}
	for _, event := range append(head, tail...) {
		if err := jr.Append(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
}

// §14 cases 8 and 22: fault injection after parallel.started, during branch
// stages, and after every settlement but before parallel.finished. Resume
// rebuilds the same skip set from the recorded event and never re-runs or
// re-reads the source task.
func TestConformanceSelectiveParallelResumeNeverRereadsSource(t *testing.T) {
	branchDone := func(id int, name string) []journal.Event {
		stage := "review-" + name
		return []journal.Event{
			{Type: journal.EventBranchStarted, Parallel: "review", Branch: id, BranchName: name, Stage: stage},
			{Type: journal.EventStageStarted, Branch: id, Stage: stage, Attempt: 1},
			{Type: journal.EventStageFinished, Branch: id, Stage: stage, Attempt: 1, Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"ran": stage}},
			{Type: journal.EventBranchFinished, Parallel: "review", Branch: id, BranchName: name, BranchStatus: journal.BranchSucceeded},
		}
	}
	boundaries := map[string][]journal.Event{
		"after parallel.started": nil,
		"during branch stages": {
			{Type: journal.EventBranchStarted, Parallel: "review", Branch: 1, BranchName: "baseline", Stage: "review-baseline"},
			{Type: journal.EventStageStarted, Branch: 1, Stage: "review-baseline", Attempt: 1},
		},
		"after every settlement": append(branchDone(1, "baseline"), branchDone(4, "ui")...),
	}
	for name, tail := range boundaries {
		for _, width := range widths() {
			t.Run(fmt.Sprintf("%s/width-%d", name, width), func(t *testing.T) {
				h := newSelectiveHarness(t, "never read", nil)
				machine := selectiveMachine(t, selectiveOptions{width: width})
				runID := fmt.Sprintf("%s-resume-%d", selectiveRunPrefix, width)
				selectiveJournal(t, h.runsDir, runID, machine, tail...)
				result, err := h.runner.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine})
				if err != nil {
					t.Fatalf("Resume: %v", err)
				}
				if result.Phase != journal.PhaseCompleted {
					t.Fatalf("phase = %q, want completed", result.Phase)
				}
				if got := h.callCount("triage"); got != 0 {
					t.Fatalf("source task re-ran %d times on resume", got)
				}
				if h.callCount("review-security") != 0 || h.callCount("review-migration") != 0 {
					t.Fatal("a skipped branch ran on resume")
				}
				events := h.events(t, runID)
				if got := len(eventsOfType(events, journal.EventParallelStarted)); got != 1 {
					t.Fatalf("parallel.started count = %d, want the original one only", got)
				}
				finished := parallelCompleteness(t, h.runsDir, runID)
				if got := completenessSummary(finished); got != "1:baseline:succeeded,2:security:skipped,3:migration:skipped,4:ui:succeeded" {
					t.Fatalf("completeness = %s", got)
				}
				if finished[3].Reason != "touches portal" || finished[1].Reason != "not selected by triage.lenses" {
					t.Fatalf("reasons lost on resume: %+v", finished)
				}
				if joinRuns(events) != 1 {
					t.Fatalf("join ran %d times", joinRuns(events))
				}
			})
		}
	}
}

// Live drain: a selective run stopped mid-branch resumes to the same skip set
// without re-running the source task.
func TestSelectiveParallelDrainAndResumeKeepsFrozenSelection(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 4)
	h := &selectiveHarness{calls: map[string]int{}}
	var (
		mu    sync.Mutex
		calls = map[string]int{}
	)
	h.runner, h.runsDir = newParallelTestRunner(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &blockingSelection{
			selectionDeterministic: selectionDeterministic{rec: rec, document: []byte(`{"schemaVersion":1,"branches":[{"name":"ui"}]}`), mu: &mu, calls: calls},
			started:                started, release: release,
		}, nil
	})
	machine := selectiveMachine(t, selectiveOptions{width: 3})
	const runID = "selective-drain"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		result, err := h.runner.Start(ctx, StartInput{RunID: runID, Gaggle: "demo", Machine: machine, Trigger: journal.Trigger{Kind: journal.TriggerManual}})
		if err == nil && result.Phase != journal.PhaseRunning {
			err = fmt.Errorf("drained phase = %q, want running", result.Phase)
		}
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(runnerTestWaitTimeout):
			t.Fatal("selected branches did not start")
		}
	}
	cancel()
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("drain did not return")
	}
	result, err := h.runner.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatalf("Resume = %+v, %v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["triage"] != 1 || calls["review-security"] != 0 || calls["review-migration"] != 0 || calls["review-ui"] != 1 || calls["review-baseline"] != 1 {
		t.Fatalf("stage calls after drain+resume = %v", calls)
	}
}

type blockingSelection struct {
	selectionDeterministic
	started chan<- string
	release <-chan struct{}
}

func (b *blockingSelection) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if strings.Contains(env.TaskID, ":review-") {
		b.started <- env.TaskID
		<-b.release
	}
	return b.selectionDeterministic.Run(ctx, env, run)
}

// §8: conflicting records refuse resume with a typed workflow_integrity
// failure and never repair themselves by re-running the source.
func TestSelectiveParallelResumeRefusesConflictingRecords(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail []journal.Event
	}{
		{"skipped branch started", []journal.Event{
			{Type: journal.EventBranchStarted, Parallel: "review", Branch: 2, BranchName: "security", Stage: "review-security"},
		}},
		{"skipped branch ran a stage", []journal.Event{
			{Type: journal.EventStageStarted, Branch: 3, Stage: "review-migration", Attempt: 1},
		}},
		{"selected branch finished as skipped", []journal.Event{
			{Type: journal.EventBranchFinished, Parallel: "review", Branch: 4, BranchName: "ui", BranchStatus: journal.BranchSkipped},
		}},
		{"finish disagrees with skip set", []journal.Event{
			{Type: journal.EventParallelFinished, Parallel: "review", Target: "collate", Completeness: []journal.BranchOutcome{
				{Branch: 1, Name: "baseline", Status: journal.BranchSucceeded}, {Branch: 2, Name: "security", Status: journal.BranchSucceeded},
				{Branch: 3, Name: "migration", Status: journal.BranchSkipped}, {Branch: 4, Name: "ui", Status: journal.BranchSucceeded},
			}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSelectiveHarness(t, "never read", nil)
			machine := selectiveMachine(t, selectiveOptions{})
			runID := selectiveRunPrefix + "-conflict"
			selectiveJournal(t, h.runsDir, runID, machine, tc.tail...)
			before := len(h.events(t, runID))
			_, err := h.runner.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine})
			var integrity *WorkflowIntegrityError
			if !errors.As(err, &integrity) || integrity.StageErrorCode() != WorkflowIntegrityCode || integrity.State != "review" {
				t.Fatalf("Resume error = %v, want typed workflow_integrity", err)
			}
			if h.callCount("triage") != 0 {
				t.Fatal("integrity refusal re-ran the source task")
			}
			if after := len(h.events(t, runID)); after != before {
				t.Fatalf("refused resume appended %d events", after-before)
			}
		})
	}
}

// recordedSkipSet rejects a parallel.started whose own selection and
// completeness contradict each other or the pinned definition.
func TestRecordedSkipSetRejectsSelfContradictoryStart(t *testing.T) {
	spec := selectiveMachine(t, selectiveOptions{}).Def.Spec.Parallels[0]
	valid := func() journal.Event {
		return journal.Event{
			Type: journal.EventParallelStarted, Parallel: "review",
			Selection: &journal.ParallelSelection{Source: journal.SelectionSource{Task: "triage", Slot: "lenses", Attempt: 1}, Selected: []string{"security", "ui"}},
			Completeness: []journal.BranchOutcome{
				{Branch: 1, Name: "baseline"}, {Branch: 2, Name: "security"},
				{Branch: 3, Name: "migration", Status: journal.BranchSkipped}, {Branch: 4, Name: "ui"},
			},
		}
	}
	if skipped, err := recordedSkipSet(spec, valid()); err != nil || len(skipped) != 1 || !skipped[3] {
		t.Fatalf("valid start = %v, %v", skipped, err)
	}
	for name, mutate := range map[string]func(*journal.Event){
		"non-optional selected":   func(e *journal.Event) { e.Selection.Selected = []string{"baseline"} },
		"unknown selected":        func(e *journal.Event) { e.Selection.Selected = []string{"performance"} },
		"duplicate selected":      func(e *journal.Event) { e.Selection.Selected = []string{"ui", "ui"} },
		"out of order":            func(e *journal.Event) { e.Selection.Selected = []string{"ui", "security"} },
		"reason for unselected":   func(e *journal.Event) { e.Selection.Reasons = map[string]string{"migration": "x"} },
		"other source":            func(e *journal.Event) { e.Selection.Source.Slot = "other" },
		"skip set mismatch":       func(e *journal.Event) { e.Completeness[2].Status = journal.BranchCancelled },
		"missing branch entry":    func(e *journal.Event) { e.Completeness = e.Completeness[:3] },
		"selected marked skipped": func(e *journal.Event) { e.Completeness[3].Status = journal.BranchSkipped },
	} {
		event := valid()
		mutate(&event)
		var integrity *WorkflowIntegrityError
		if _, err := recordedSkipSet(spec, event); !errors.As(err, &integrity) {
			t.Errorf("%s: err = %v, want workflow_integrity", name, err)
		}
	}
	// A pinned selective parallel whose start carries no selection (or a
	// plain parallel whose start carries one) is equally conflicting.
	machine := selectiveMachine(t, selectiveOptions{})
	started := valid()
	started.Selection = nil
	var integrity *WorkflowIntegrityError
	if err := verifyRecordedSelections([]journal.Event{started}, machine); !errors.As(err, &integrity) {
		t.Fatalf("missing selection: err = %v", err)
	}
	plain := parallelRunnerMachine(t, 1, apiv1.WorkspaceScratch)
	extra := journal.Event{Type: journal.EventParallelStarted, Parallel: "fan", Selection: valid().Selection}
	if err := verifyRecordedSelections([]journal.Event{extra}, plain); !errors.As(err, &integrity) {
		t.Fatalf("extra selection: err = %v", err)
	}
}

// A human rerun cannot reach a stage in a branch the frozen selection skipped.
func TestRefuseSkippedBranchRerun(t *testing.T) {
	spec := selectiveMachine(t, selectiveOptions{}).Def.Spec.Parallels[0]
	par := newParallelExec(spec)
	par.applySelection(&journal.ParallelSelection{Source: journal.SelectionSource{Task: "triage", Slot: "lenses"}, Selected: []string{"ui"}})
	var integrity *WorkflowIntegrityError
	if err := refuseSkippedBranchRerun(par, par.branch("security")); !errors.As(err, &integrity) {
		t.Fatalf("rerun into skipped branch: err = %v", err)
	}
	if err := refuseSkippedBranchRerun(par, par.branch("ui")); err != nil {
		t.Fatalf("rerun into selected branch refused: %v", err)
	}
}
