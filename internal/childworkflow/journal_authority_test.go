package childworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

type authorityFixture struct {
	run      *journal.Run
	id       journal.RunIdentity
	pinned   PinnedStageAdmission
	resolver *JournalAuthorityResolver
}

func newAuthorityFixture(t *testing.T) *authorityFixture {
	t.Helper()
	admission := testContext()
	admission.ConfigDigest = digest([]byte("config"))
	admission.ParentTask.Goober = "coder"
	admission.ParentTask.Goal = "Delegate a bounded child"
	admission.ParentTask.Capabilities = []string{"agent:model", "repo:read"}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "factory", Version: 1, DSLVersion: "3.1",
		Spec: apiv1.WorkflowSpec{Gaggle: "web", Start: "plan", Tasks: []apiv1.Task{admission.ParentTask},
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	admission.ParentTask, _ = machine.Task("plan")
	definition, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	id := journal.RunIdentity{RunID: "parent-run", InstanceID: "instance", Workflow: "factory", WorkflowVersion: 1,
		Gaggle: "web", WorkflowDigest: machine.Digest(), ConfigGeneration: digest([]byte("generation")), GooberDigest: digest([]byte("goobers"))}
	run, err := journal.Create(t.TempDir(), id, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
		journal.WithInputIntegrity(map[string]apiv1.Integrity{journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	f := &authorityFixture{run: run, id: id, pinned: PinnedStageAdmission{Admission: admission, ConfigGeneration: id.ConfigGeneration,
		WorkflowDigest: id.WorkflowDigest, GooberDigest: id.GooberDigest}}
	f.resolver = &JournalAuthorityResolver{
		OpenJournal: func(_ context.Context, runID string) (*journal.Reader, error) {
			if runID != id.RunID {
				return nil, ErrAuthorityUnavailable
			}
			return journal.OpenReadOnly(run.Dir())
		},
		LoadPinnedStage: func(_ context.Context, identity journal.RunIdentity, stage string) (PinnedStageAdmission, error) {
			if identity.ConfigGeneration != id.ConfigGeneration || stage != "plan" {
				return PinnedStageAdmission{}, ErrAuthorityUnavailable
			}
			return f.pinned, nil
		},
	}
	return f
}

func (f *authorityFixture) start(t *testing.T, branch, attempt int, continuation bool) apiv1.InvocationEnvelope {
	t.Helper()
	_, origin, err := f.run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Branch: branch,
		Stage: "plan", Attempt: attempt, Runner: map[string]any{"goober": "coder"}}, continuation)
	if err != nil {
		t.Fatal(err)
	}
	return apiv1.InvocationEnvelope{TaskID: f.id.RunID + ":plan", RunID: f.id.RunID, InstanceID: f.id.InstanceID,
		WorkflowID: f.id.Workflow, Gaggle: f.id.Gaggle, Attempt: int32(attempt), Goober: "coder", GooberDigest: f.id.GooberDigest,
		ConfigGeneration: f.id.ConfigGeneration, Capabilities: []string{"agent:model", "repo:read"}, ChildWorkflowOrigin: origin}
}

func (f *authorityFixture) append(t *testing.T, event journal.Event) {
	t.Helper()
	if err := f.run.Append(event); err != nil {
		t.Fatal(err)
	}
}

func requireNoAuthority(t *testing.T, authority Authority, err error) {
	t.Helper()
	if err == nil || authority.Origin.RunID != "" {
		t.Fatalf("unexpected authority: %+v, %v", authority.Origin, err)
	}
}

func TestJournalAuthorityRoundTripAndIndependentSnapshot(t *testing.T) {
	f := newAuthorityFixture(t)
	envelope := f.start(t, 1, 1, false)
	authority, err := f.resolver.PrepareStage(t.Context(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if authority.Origin.GrantID != "" || authority.ConfigGeneration != f.id.ConfigGeneration || authority.Actor == "" || authority.ParentWorkflow != f.id.Workflow ||
		authority.ParentWorkflowDigest != f.id.WorkflowDigest || authority.ParentGooberDigest != f.id.GooberDigest ||
		authority.Origin.PolicyDigest != validator(t, f.pinned.Admission).PolicyDigest() {
		t.Fatalf("wrong prepared scope: %+v", authority)
	}
	origin := authority.Origin
	origin.GrantID = "signed-grant"
	resolved, err := f.resolver.Resolve(t.Context(), origin)
	if err != nil || resolved.Origin != origin || resolved.Actor != authority.Actor {
		t.Fatalf("resolve: %+v, %v", resolved, err)
	}
	f.pinned.Admission.ParentTask.ChildWorkflows.AllowedGoobers[0] = "mutated"
	f.pinned.Admission.Goobers["coder"] = apiv1.GooberSpec{}
	f.pinned.Admission.GrantedCapabilities[0] = "repo:write"
	if authority.Admission.ParentTask.ChildWorkflows.AllowedGoobers[0] != "coder" ||
		authority.Admission.Goobers["coder"].Harness != "fake" || authority.Admission.GrantedCapabilities[0] != "agent:model" {
		t.Fatal("prepared authority aliases the loader's mutable policy")
	}
}

func TestJournalAuthorityParallelRetryAndRevisit(t *testing.T) {
	f := newAuthorityFixture(t)
	first := f.start(t, 1, 1, false)
	if _, err := f.resolver.PrepareStage(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	sibling := f.start(t, 2, 1, false)
	f.append(t, journal.Event{Type: journal.EventStageFinished, Branch: 2, Stage: "plan", Attempt: 1})
	if _, err := f.resolver.PrepareStage(t.Context(), first); err != nil {
		t.Fatalf("sibling completion revoked live stage: %v", err)
	}
	authority, err := f.resolver.PrepareStage(t.Context(), sibling)
	requireNoAuthority(t, authority, err)
	retry := f.start(t, 1, 2, true)
	authority, err = f.resolver.PrepareStage(t.Context(), first)
	requireNoAuthority(t, authority, err)
	// Late completion of the earlier attempt must not terminate its replacement.
	f.append(t, journal.Event{Type: journal.EventStageFinished, Branch: 1, Stage: "plan", Attempt: 1})
	active, err := f.resolver.PrepareStage(t.Context(), retry)
	if err != nil || retry.ChildWorkflowOrigin.StageOccurrence != first.ChildWorkflowOrigin.StageOccurrence {
		t.Fatalf("retry: %+v, %v", active, err)
	}
	f.append(t, journal.Event{Type: journal.EventStageFinished, Branch: 1, Stage: "plan", Attempt: 2})
	revisit := f.start(t, 1, 1, false)
	authority, err = f.resolver.PrepareStage(t.Context(), retry)
	requireNoAuthority(t, authority, err)
	current, err := f.resolver.PrepareStage(t.Context(), revisit)
	if err != nil || current.Origin.StageOccurrence == active.Origin.StageOccurrence || current.Actor == active.Actor {
		t.Fatalf("new visit reused old authority: %+v, %v", current, err)
	}
}

func TestJournalAuthorityResumeRequiresNewCommittedAttempt(t *testing.T) {
	f := newAuthorityFixture(t)
	first := f.start(t, 0, 1, false)
	f.append(t, journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)})
	authority, err := f.resolver.PrepareStage(t.Context(), first)
	requireNoAuthority(t, authority, err)
	f.append(t, journal.Event{Type: journal.EventRunResumed})
	authority, err = f.resolver.PrepareStage(t.Context(), first)
	requireNoAuthority(t, authority, err)
	resumed := f.start(t, 0, 2, true)
	if _, err := f.resolver.PrepareStage(t.Context(), resumed); err != nil {
		t.Fatal(err)
	}
}

func TestJournalAuthorityRefusesScopeAndMissingProvenance(t *testing.T) {
	tests := []struct {
		name string
		edit func(*authorityFixture, *apiv1.InvocationEnvelope)
	}{
		{"missing origin", func(_ *authorityFixture, e *apiv1.InvocationEnvelope) { e.ChildWorkflowOrigin = nil }},
		{"wrong run", func(_ *authorityFixture, e *apiv1.InvocationEnvelope) { e.RunID = "../parent-run" }},
		{"wrong gaggle", func(_ *authorityFixture, e *apiv1.InvocationEnvelope) { e.Gaggle = "another" }},
		{"wrong instance", func(_ *authorityFixture, e *apiv1.InvocationEnvelope) { e.InstanceID = "another" }},
		{"wrong workflow", func(_ *authorityFixture, e *apiv1.InvocationEnvelope) { e.WorkflowID = "another" }},
		{"wrong stage", func(_ *authorityFixture, e *apiv1.InvocationEnvelope) { e.TaskID = "plan" }},
		{"wrong attempt", func(_ *authorityFixture, e *apiv1.InvocationEnvelope) { e.Attempt++ }},
		{"wrong goober", func(_ *authorityFixture, e *apiv1.InvocationEnvelope) { e.Goober = "another" }},
		{"wrong kit", func(_ *authorityFixture, e *apiv1.InvocationEnvelope) { e.GooberDigest = digest([]byte("other")) }},
		{"wrong generation", func(_ *authorityFixture, e *apiv1.InvocationEnvelope) { e.ConfigGeneration = digest([]byte("other")) }},
		{"missing grant", func(_ *authorityFixture, e *apiv1.InvocationEnvelope) { e.Capabilities = nil }},
		{"extra grant", func(_ *authorityFixture, e *apiv1.InvocationEnvelope) {
			e.Capabilities = append(e.Capabilities, "repo:write")
		}},
		{"duplicate grant", func(_ *authorityFixture, e *apiv1.InvocationEnvelope) {
			e.Capabilities = append(e.Capabilities, "repo:read")
		}},
		{"missing archive", func(f *authorityFixture, _ *apiv1.InvocationEnvelope) { f.resolver.LoadPinnedStage = nil }},
		{"wrong archive pin", func(f *authorityFixture, _ *apiv1.InvocationEnvelope) {
			f.pinned.ConfigGeneration = digest([]byte("other"))
		}},
		{"wrong workflow pin", func(f *authorityFixture, _ *apiv1.InvocationEnvelope) {
			f.pinned.WorkflowDigest = digest([]byte("other"))
		}},
		{"wrong kit pin", func(f *authorityFixture, _ *apiv1.InvocationEnvelope) {
			f.pinned.GooberDigest = digest([]byte("other"))
		}},
		{"edited policy", func(f *authorityFixture, _ *apiv1.InvocationEnvelope) {
			f.pinned.Admission.ParentTask.ChildWorkflows.MaxChildren = 10
		}},
		{"missing opt in", func(f *authorityFixture, _ *apiv1.InvocationEnvelope) {
			f.pinned.Admission.ParentTask.ChildWorkflows = nil
		}},
		{"publication elevation", func(f *authorityFixture, _ *apiv1.InvocationEnvelope) { f.pinned.Admission.AllowPRPublication = true }},
		{"grant elevation", func(f *authorityFixture, e *apiv1.InvocationEnvelope) {
			f.pinned.Admission.GrantedCapabilities = append(f.pinned.Admission.GrantedCapabilities, "repo:write")
			e.Capabilities = append(e.Capabilities, "repo:write")
		}},
		{"backend mismatch", func(f *authorityFixture, _ *apiv1.InvocationEnvelope) { f.pinned.Admission.Backend = BackendEngine }},
		{"missing journal", func(f *authorityFixture, _ *apiv1.InvocationEnvelope) {
			f.resolver.OpenJournal = func(context.Context, string) (*journal.Reader, error) { return nil, errors.New("unavailable") }
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newAuthorityFixture(t)
			envelope := f.start(t, 0, 1, false)
			test.edit(f, &envelope)
			authority, err := f.resolver.PrepareStage(t.Context(), envelope)
			requireNoAuthority(t, authority, err)
		})
	}
}

func TestJournalAuthorityRechecksPolicyAndJournalAfterLoad(t *testing.T) {
	f := newAuthorityFixture(t)
	envelope := f.start(t, 0, 1, false)
	authority, err := f.resolver.PrepareStage(t.Context(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	origin := authority.Origin
	origin.GrantID = "signed-grant"
	f.pinned.Admission.GrantedCapabilities = []string{"repo:read"}
	authority, err = f.resolver.Resolve(t.Context(), origin)
	requireNoAuthority(t, authority, err)
	// A new envelope can use narrowed permissions; the old signed policy cannot.
	envelope.Capabilities = []string{"repo:read"}
	if _, err := f.resolver.PrepareStage(t.Context(), envelope); err != nil {
		t.Fatal(err)
	}
	f.resolver.LoadPinnedStage = func(context.Context, journal.RunIdentity, string) (PinnedStageAdmission, error) {
		f.append(t, journal.Event{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1})
		return f.pinned, nil
	}
	authority, err = f.resolver.PrepareStage(t.Context(), envelope)
	requireNoAuthority(t, authority, err)
	if !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("lost race was not reported: %v", err)
	}
}

func TestJournalAuthorityRefusesUnboundStageAndBorrowedOccurrence(t *testing.T) {
	f := newAuthorityFixture(t)
	sibling := f.start(t, 1, 1, false)
	// Even a well-formed attempt hash cannot claim a sibling's occurrence.
	attempt := journal.StageAttemptID(f.id.RunID, 2, "plan", 2)
	f.append(t, journal.Event{Type: journal.EventStageStarted, Branch: 2, Stage: "plan", Attempt: 1,
		Runner: map[string]any{"goober": "coder", journal.ChildWorkflowOccurrenceKey: sibling.ChildWorkflowOrigin.StageOccurrence,
			journal.ChildWorkflowAttemptKey: attempt}})
	borrowed := sibling
	borrowed.ChildWorkflowOrigin = &apiv1.ChildWorkflowOrigin{StageOccurrence: sibling.ChildWorkflowOrigin.StageOccurrence, AttemptID: attempt}
	authority, err := f.resolver.PrepareStage(t.Context(), borrowed)
	requireNoAuthority(t, authority, err)
	// A legacy unbound start replaces the active stage; its presence never mints
	// missing provenance for either the old or new attempt.
	f.append(t, journal.Event{Type: journal.EventStageStarted, Branch: 1, Stage: "plan", Attempt: 2, Runner: map[string]any{"goober": "coder"}})
	authority, err = f.resolver.PrepareStage(t.Context(), sibling)
	requireNoAuthority(t, authority, err)
}

func TestJournalAuthorityRequiresTrustedPinnedDefinition(t *testing.T) {
	for _, mode := range []string{"missing", "untrusted", "wrong digest", "wrong version"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuthorityFixture(t)
			f.start(t, 0, 1, false)
			rd, err := journal.OpenReadOnly(f.run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			id, err := rd.Identity()
			if err != nil {
				t.Fatal(err)
			}
			events, err := rd.Events()
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing":
				id.Inputs = nil
			case "untrusted":
				id.Inputs[0].Integrity = apiv1.IntegrityUnapproved
			case "wrong digest":
				id.WorkflowDigest = digest([]byte("tampered"))
				f.pinned.WorkflowDigest = id.WorkflowDigest
			case "wrong version":
				id.WorkflowVersion++
			}
			if validator, err := checkedPinnedStage(rd, id, events[0], f.pinned); err == nil || validator != nil {
				t.Fatal("accepted missing or altered pinned definition")
			}
		})
	}
}

func TestJournalAuthorityControlTransitionsEndAuthority(t *testing.T) {
	for _, kind := range []journal.EventType{journal.EventStageFinished, journal.EventStageRerunRequested,
		journal.EventBranchFinished, journal.EventGateStarted, journal.EventGatePaused, journal.EventParallelStarted} {
		t.Run(string(kind), func(t *testing.T) {
			f := newAuthorityFixture(t)
			envelope := f.start(t, 2, 1, false)
			f.append(t, journal.Event{Type: kind, Stage: "plan", Branch: 2, Attempt: 1})
			authority, err := f.resolver.PrepareStage(t.Context(), envelope)
			requireNoAuthority(t, authority, err)
		})
	}
}

func TestJournalAuthorityCancelledLookupAndPolicyDigestSnapshot(t *testing.T) {
	f := newAuthorityFixture(t)
	envelope := f.start(t, 0, 1, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	authority, err := f.resolver.PrepareStage(ctx, envelope)
	requireNoAuthority(t, authority, err)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost context cancellation: %v", err)
	}
	v := validator(t, f.pinned.Admission)
	before := v.PolicyDigest()
	f.pinned.Admission.ParentTask.ChildWorkflows.MaxChildren = 20
	if before != v.PolicyDigest() {
		t.Fatal("policy digest changed after mutation of constructor input")
	}
	proposal, err := v.Validate([]byte(validProposal))
	if err != nil || proposal.PolicyDigest != before {
		t.Fatalf("proposal digest differs from pre-mint digest: %v", err)
	}
}

func TestJournalAuthorityParallelJoinEndsBranchAuthority(t *testing.T) {
	f := newAuthorityFixture(t)
	f.append(t, journal.Event{Type: journal.EventBranchStarted, Branch: 1, Parallel: "fanout"})
	envelope := f.start(t, 1, 1, false)
	if _, err := f.resolver.PrepareStage(t.Context(), envelope); err != nil {
		t.Fatal(err)
	}
	f.append(t, journal.Event{Type: journal.EventParallelFinished, Parallel: "fanout"})
	authority, err := f.resolver.PrepareStage(t.Context(), envelope)
	requireNoAuthority(t, authority, err)
}

func TestJournalAuthorityAllowsNarrowerDelegatableCapabilities(t *testing.T) {
	f := newAuthorityFixture(t)
	envelope := f.start(t, 0, 1, false)
	// The agent may retain its own model permission while delegation is limited
	// to reading the repo. The grant must never widen a reduced native envelope.
	f.pinned.Admission.GrantedCapabilities = []string{"repo:read"}
	if _, err := f.resolver.PrepareStage(t.Context(), envelope); err != nil {
		t.Fatalf("child subset refused the enclosing parent grant: %v", err)
	}
	envelope.Capabilities = []string{"repo:read"}
	if _, err := f.resolver.PrepareStage(t.Context(), envelope); err != nil {
		t.Fatalf("valid native narrowing refused: %v", err)
	}
	envelope.Capabilities = []string{"agent:model"}
	authority, err := f.resolver.PrepareStage(t.Context(), envelope)
	requireNoAuthority(t, authority, err)
}
