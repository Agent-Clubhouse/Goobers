package childengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/podauth"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// parentFixture is an engine parent run as control sees it: its start-fence
// pins, its worker-written journal, its live engine execution and the retained
// generation it was started under.
type parentFixture struct {
	pins       acceptance.RunPins
	identity   journal.RunIdentity
	run        *journal.Run
	origin     apiv1.ChildWorkflowOrigin
	execution  startfence.StartedExecution
	describe   error
	source     treeSource
	applied    appliedConfig
	definition []byte
}

// parentInstance is the control plane's instance in the fixture; the
// PostgreSQL tests use the same instance for custody. It must be a valid
// durable instance ID so production dispatch stamps its pod identity label.
const parentInstance = "0123456789abcdef0123456789abcdef"

// fixtureGeneration is a well-formed generation digest; the tree it names is
// the fixture's retained configuration.
const fixtureGeneration = "sha256:4f1c3b0a9e8d7c6b5a49382716051f0e2d3c4b5a69788796a5b4c3d2e1f00112"

func newParentFixture(t *testing.T) *parentFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "demo")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	layout := instance.NewLayout(root)
	parentFile := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(parentFile, []byte(childEnabledParent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	retainedDir := filepath.Join(t.TempDir(), "retained-generation")
	copyTree(t, layout.ConfigDir(), retainedDir)
	retained := loadSet(t, retainedDir)
	workflowDigest, gooberDigest, _ := parentPins(t, cfg, retainedDir, retained)
	selected := childworkflow.ParentSelection{Gaggle: "example", Workflow: "default-implement", Stage: "plan"}
	_, machine, err := childworkflow.StageCatalog(cfg, retained, selected, childworkflow.BackendEngine, childworkflow.OfflineGooberAdmission)
	if err != nil {
		t.Fatal(err)
	}
	f := &parentFixture{
		pins: acceptance.RunPins{RunID: strings.Repeat("a", 32), AcceptanceID: "trigger-" + strings.Repeat("a", 32), Gaggle: "example",
			Workflow: "default-implement", WorkflowVersion: machine.Def.Version, WorkflowDigest: workflowDigest, GooberDigest: gooberDigest,
			ConfigGeneration: fixtureGeneration, BranchNamespace: providers.NormalizeBranchNamespace(""), InputDigest: "sha256:started-input"},
		source:  treeSource{generation: fixtureGeneration, directory: retainedDir, set: retained},
		applied: appliedConfig{candidate: cloudconfig.Candidate{Digest: "sha256:live-generation", Config: cfg, Set: retained}, ok: true},
	}
	// The start resolver pins the gaggle's project from the started generation.
	f.pins.RepoRef = gaggleProject(t, retained, "example")
	f.execution = startfence.StartedExecution{Status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, Engine: true,
		Owner: &startfence.StartOwnerMemo{AcceptanceID: f.pins.AcceptanceID, InputDigest: f.pins.InputDigest}}
	repo := f.pins.RepoRef
	f.identity = journal.RunIdentity{InstanceID: parentInstance, RunID: f.pins.RunID, Workflow: f.pins.Workflow, WorkflowVersion: f.pins.WorkflowVersion,
		Gaggle: f.pins.Gaggle, WorkflowDigest: f.pins.WorkflowDigest, GooberDigest: f.pins.GooberDigest, ConfigGeneration: f.pins.ConfigGeneration,
		Driver: journal.DriverEngine, WorkspaceRepository: &repo}
	if f.definition, err = json.Marshal(machine.Def); err != nil {
		t.Fatal(err)
	}
	f.run, f.origin = startParentJournal(t, f.identity, f.definition)
	return f
}

func gaggleProject(t *testing.T, set *instance.ConfigSet, name string) apiv1.RepoRef {
	t.Helper()
	for _, gaggle := range set.Gaggles {
		if gaggle.Name == name {
			return gaggle.Spec.Project
		}
	}
	t.Fatalf("no gaggle %q", name)
	return apiv1.RepoRef{}
}

func startParentJournal(t *testing.T, id journal.RunIdentity, definition []byte) (*journal.Run, apiv1.ChildWorkflowOrigin) {
	t.Helper()
	run, err := journal.Create(t.TempDir(), id, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
		journal.WithInputIntegrity(map[string]apiv1.Integrity{journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	_, origin, err := run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Stage: "plan", Attempt: 1,
		Runner: map[string]any{"goober": "coder"}}, false)
	if err != nil || origin == nil {
		t.Fatalf("start parent stage: %v, %v", origin, err)
	}
	return run, *origin
}

func (f *parentFixture) authority() ParentAuthority {
	return ParentAuthority{
		InstanceID: parentInstance,
		Pins: func(_ context.Context, runID string) (acceptance.RunPins, error) {
			if runID != f.pins.RunID {
				return acceptance.RunPins{}, sql.ErrNoRows
			}
			return f.pins, nil
		},
		Journal: func(_ context.Context, runID string) (journal.RunReader, error) {
			if runID != f.identity.RunID {
				return nil, os.ErrNotExist
			}
			return journal.OpenReadOnly(f.run.Dir())
		},
		Execution: func(context.Context, string) (startfence.StartedExecution, error) {
			return f.execution, f.describe
		},
		Generations: f.source,
		Applied:     f.applied,
	}
}

func TestParentAuthorityBuildsTheParentFromPinsAndBindsThePinnedRepository(t *testing.T) {
	f := newParentFixture(t)
	authority, err := f.authority().PrepareOrigin(t.Context(), f.pins.RunID, f.origin)
	if err != nil {
		t.Fatal(err)
	}
	o := authority.Origin
	switch {
	case o.GrantID != "" || o.RunID != f.pins.RunID || o.Gaggle != "example" || o.StageOccurrence != f.origin.StageOccurrence || o.AttemptID != f.origin.AttemptID:
		t.Fatalf("origin = %+v", o)
	case authority.ConfigGeneration != fixtureGeneration || authority.ParentWorkflowDigest != f.pins.WorkflowDigest || authority.ParentGooberDigest != f.pins.GooberDigest:
		t.Fatalf("parent pins = %+v", authority)
	case authority.Admission.Backend != childworkflow.BackendEngine:
		t.Fatalf("backend = %q", authority.Admission.Backend)
	case authority.Execution == nil || authority.Execution.InstanceID != parentInstance || authority.Execution.RepoRef != f.pins.RepoRef:
		t.Fatalf("execution = %+v, want the pinned repository on this instance", authority.Execution)
	}
	resolved, err := f.authority().Resolve(t.Context(), withGrant(o))
	if err != nil || resolved.Origin != withGrant(o) || resolved.Execution == nil || resolved.Execution.RepoRef != f.pins.RepoRef {
		t.Fatalf("Resolve = %+v, %v", resolved, err)
	}
}

func withGrant(o childworkflow.Origin) childworkflow.Origin {
	o.GrantID = "grant-1"
	return o
}

// Each pin the bind needs, left empty by the start, refuses the bind, and says
// which: pins are evidence, never a start gate.
func TestParentAuthorityRefusesEachEmptyRequiredPin(t *testing.T) {
	for name, tc := range map[string]struct {
		clear   func(*acceptance.RunPins)
		missing string
	}{
		"gaggle":            {func(p *acceptance.RunPins) { p.Gaggle = "" }, "gaggle"},
		"workflow":          {func(p *acceptance.RunPins) { p.Workflow = "" }, "workflow"},
		"config generation": {func(p *acceptance.RunPins) { p.ConfigGeneration = "" }, "config generation"},
		"workflow digest":   {func(p *acceptance.RunPins) { p.WorkflowDigest = "" }, "workflow digest"},
		"goober digest":     {func(p *acceptance.RunPins) { p.GooberDigest = "" }, "goober digest"},
		"repository":        {func(p *acceptance.RunPins) { p.RepoRef = apiv1.RepoRef{} }, "repository"},
		"repository name":   {func(p *acceptance.RunPins) { p.RepoRef.Name = "" }, "repository"},
		"input digest":      {func(p *acceptance.RunPins) { p.InputDigest = "" }, "input digest"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newParentFixture(t)
			tc.clear(&f.pins)
			err := refusal(t, f)
			if !strings.Contains(err.Error(), "no pinned "+tc.missing) {
				t.Fatalf("refusal %q does not name the missing %s pin", err, tc.missing)
			}
		})
	}
}

// A pinned repository other than the pinned generation's gaggle project is
// refused even when the worker's journal agrees with it: children never act
// on a repository the configuration did not name.
func TestParentAuthorityRefusesARepositoryThatIsNotTheGaggleProject(t *testing.T) {
	f := newParentFixture(t)
	f.pins.RepoRef = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "elsewhere", Branch: "main"}
	id := f.identity
	repo := f.pins.RepoRef
	id.WorkspaceRepository = &repo
	f.run, f.origin = startParentJournal(t, id, f.definition)
	err := refusal(t, f)
	if !strings.Contains(err.Error(), "not its generation's gaggle project") {
		t.Fatalf("refusal %q is not the repository check", err)
	}
}

func TestParentAuthorityRefusesARunWithoutPins(t *testing.T) {
	f := newParentFixture(t)
	_, err := f.authority().PrepareOrigin(t.Context(), strings.Repeat("b", 32), f.origin)
	if !errors.Is(classify(err), engine.ErrChildCustodyRefused) {
		t.Fatalf("unpinned run = %v, want a refusal", err)
	}
}

// The worker-written journal must agree with the pins; it never supplies
// authority.
func TestParentAuthorityRefusesAJournalThatDisagreesWithItsPins(t *testing.T) {
	pinEdits := map[string]func(*acceptance.RunPins){
		"workflow version": func(p *acceptance.RunPins) { p.WorkflowVersion++ },
		"repository":       func(p *acceptance.RunPins) { p.RepoRef.Name = "elsewhere" },
	}
	for name, edit := range pinEdits {
		t.Run("pin "+name, func(t *testing.T) {
			f := newParentFixture(t)
			edit(&f.pins)
			requireRefused(t, f)
		})
	}
	journalEdits := map[string]func(*journal.RunIdentity){
		"instance":     func(id *journal.RunIdentity) { id.InstanceID = "instance-b" },
		"local driver": func(id *journal.RunIdentity) { id.Driver = "" },
		"child lineage": func(id *journal.RunIdentity) {
			id.Child = &journal.ChildLineage{Gaggle: id.Gaggle, ParentRunID: strings.Repeat("c", 32), ParentWorkflow: "outer",
				StageOccurrence: "outer-occurrence", InvocationKey: "call-1", AcceptanceID: "trigger-" + id.RunID,
				SourceDigest: fixtureGeneration, EnvelopeDigest: fixtureGeneration}
		},
		"workspace repository": func(id *journal.RunIdentity) {
			id.WorkspaceRepository = &apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "other"}
		},
	}
	for name, edit := range journalEdits {
		t.Run("journal "+name, func(t *testing.T) {
			f := newParentFixture(t)
			id := f.identity
			edit(&id)
			f.run, f.origin = startParentJournal(t, id, f.definition)
			err := refusal(t, f)
			// A local (driverless) journal passes the shared active-stage
			// check, so the refusal must be the pin cross-check's.
			if name == "local driver" && !strings.Contains(err.Error(), "disagrees with its start-fence pins") {
				t.Fatalf("local-driver journal refused by %v, want the journal/pin disagreement", err)
			}
		})
	}
	t.Run("journal without a workspace repository", func(t *testing.T) {
		f := newParentFixture(t)
		id := f.identity
		id.WorkspaceRepository = nil
		f.run, f.origin = startParentJournal(t, id, f.definition)
		authority, err := f.authority().PrepareOrigin(t.Context(), f.pins.RunID, f.origin)
		if err != nil || authority.Execution.RepoRef != f.pins.RepoRef {
			t.Fatalf("an unset journal repository must defer to the pin: %+v, %v", authority.Execution, err)
		}
	})
}

// The live engine execution must be the running, exact pinned start.
func TestParentAuthorityRequiresTheBoundRunningExecution(t *testing.T) {
	for name, edit := range map[string]func(*parentFixture){
		"completed":         func(f *parentFixture) { f.execution.Status = enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED },
		"paused":            func(f *parentFixture) { f.execution.Status = enumspb.WORKFLOW_EXECUTION_STATUS_PAUSED },
		"not the engine":    func(f *parentFixture) { f.execution.Engine = false },
		"pre-digest memo":   func(f *parentFixture) { f.execution.Owner.InputDigest = "" },
		"no start owner":    func(f *parentFixture) { f.execution.Owner = nil },
		"different start":   func(f *parentFixture) { f.execution.Owner.InputDigest = "sha256:another-input" },
		"other acceptance":  func(f *parentFixture) { f.execution.Owner.AcceptanceID = "trigger-other" },
		"execution unknown": func(f *parentFixture) { f.describe = serviceerror.NewNotFound("gone") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newParentFixture(t)
			edit(f)
			requireRefused(t, f)
		})
	}
	f := newParentFixture(t)
	f.describe = serviceerror.NewUnavailable("temporal down")
	_, err := f.authority().PrepareOrigin(t.Context(), f.pins.RunID, f.origin)
	if !errors.Is(classify(err), engine.ErrChildCustodyUnavailable) {
		t.Fatalf("an unreachable engine = %v, want a retryable unavailability", err)
	}
}

func TestParentAuthorityRefusesABranchNamespaceTheGenerationDoesNotDerive(t *testing.T) {
	f := newParentFixture(t)
	f.pins.BranchNamespace = "someone-else"
	requireRefused(t, f)
}

func TestParentAuthorityRefusesAFinishedOrUnknownAttempt(t *testing.T) {
	f := newParentFixture(t)
	borrowed := f.origin
	borrowed.AttemptID = "not-committed"
	if _, err := f.authority().PrepareOrigin(t.Context(), f.pins.RunID, borrowed); err == nil {
		t.Fatal("an uncommitted attempt prepared authority")
	}
	if err := f.run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	requireRefused(t, f)
}

func requireRefused(t *testing.T, f *parentFixture) {
	t.Helper()
	_ = refusal(t, f)
}

// refusal requires a permanent refusal and returns it.
func refusal(t *testing.T, f *parentFixture) error {
	t.Helper()
	authority, err := f.authority().PrepareOrigin(t.Context(), f.pins.RunID, f.origin)
	if err == nil || authority.Origin.RunID != "" {
		t.Fatalf("prepared %+v, %v; want a refusal", authority.Origin, err)
	}
	if !errors.Is(classify(err), engine.ErrChildCustodyRefused) {
		t.Fatalf("error %v is not a permanent refusal", err)
	}
	return err
}

func TestStageGrantTTLIsTheStageDurationPlusMarginCapped(t *testing.T) {
	limited := func(task apiv1.Task) childworkflow.Authority {
		return childworkflow.Authority{Admission: childworkflow.AdmissionContext{ParentTask: task}}
	}
	for name, tc := range map[string]struct {
		task apiv1.Task
		want time.Duration
	}{
		"timeout":            {apiv1.Task{TimeoutSeconds: 600}, 10*time.Minute + stageGrantMargin},
		"limit":              {apiv1.Task{Limits: &apiv1.Limits{MaxDurationSeconds: 1200}}, 20*time.Minute + stageGrantMargin},
		"timeout over limit": {apiv1.Task{TimeoutSeconds: 600, Limits: &apiv1.Limits{MaxDurationSeconds: 1200}}, 10*time.Minute + stageGrantMargin},
		"default":            {apiv1.Task{}, dispatcher.DefaultStageTimeout + stageGrantMargin},
		"capped":             {apiv1.Task{TimeoutSeconds: 7 * 24 * 3600}, podauth.MaxChildWorkflowGrantTTL},
	} {
		if got := StageGrantTTL(limited(tc.task)); got != tc.want {
			t.Errorf("%s: TTL = %v, want %v", name, got, tc.want)
		}
	}
}

// A run with no pins yet is a retry only while its start is being
// confirmed: its engine execution runs the start a still-dispatching
// acceptance made. Every other unpinned run is refused (#8085 c review).
func TestUnpinnedRunIsRetryableOnlyWhileItsStartIsConfirming(t *testing.T) {
	for name, tc := range map[string]struct {
		edit      func(*parentFixture, *ParentAuthority)
		retryable bool
	}{
		"start still dispatching": {func(_ *parentFixture, p *ParentAuthority) {
			p.StartPending = func(context.Context, string) (bool, error) { return true, nil }
		}, true},
		"start settled": {func(_ *parentFixture, p *ParentAuthority) {
			p.StartPending = func(context.Context, string) (bool, error) { return false, nil }
		}, false},
		"no start-state reader": {func(*parentFixture, *ParentAuthority) {}, false},
		"execution not running": {func(f *parentFixture, p *ParentAuthority) {
			f.execution.Status = enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED
			p.StartPending = func(context.Context, string) (bool, error) { return true, nil }
		}, false},
		"execution has no start owner": {func(f *parentFixture, p *ParentAuthority) {
			f.execution.Owner = nil
			p.StartPending = func(context.Context, string) (bool, error) { return true, nil }
		}, false},
		// Could not tell: retry, never refuse (nothing is granted without pins).
		"start state unreadable": {func(_ *parentFixture, p *ParentAuthority) {
			p.StartPending = func(context.Context, string) (bool, error) { return false, errors.New("connection reset") }
		}, true},
		"execution unreadable": {func(f *parentFixture, p *ParentAuthority) {
			f.describe = serviceerror.NewUnavailable("temporal down")
			p.StartPending = func(context.Context, string) (bool, error) { return true, nil }
		}, true},
		"execution unknown to the engine": {func(f *parentFixture, p *ParentAuthority) {
			f.describe = serviceerror.NewNotFound("gone")
			p.StartPending = func(context.Context, string) (bool, error) { return true, nil }
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newParentFixture(t)
			authority := f.authority()
			authority.Pins = func(context.Context, string) (acceptance.RunPins, error) { return acceptance.RunPins{}, sql.ErrNoRows }
			var asked []string
			tc.edit(f, &authority)
			if pending := authority.StartPending; pending != nil {
				authority.StartPending = func(ctx context.Context, id string) (bool, error) {
					asked = append(asked, id)
					return pending(ctx, id)
				}
			}
			_, err := authority.PrepareOrigin(t.Context(), f.pins.RunID, f.origin)
			if retryable := errors.Is(classify(err), engine.ErrChildCustodyUnavailable); retryable != tc.retryable || err == nil {
				t.Fatalf("unpinned run = %v (retryable %v), want retryable %v", err, retryable, tc.retryable)
			}
			if name == "start still dispatching" && (len(asked) != 1 || asked[0] != f.pins.AcceptanceID) {
				t.Fatalf("start state asked for %v, want the memo's acceptance %s", asked, f.pins.AcceptanceID)
			}
		})
	}
}
