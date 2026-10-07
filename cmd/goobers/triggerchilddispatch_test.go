package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

const dispatchChildSource = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: generated-check
spec:
  gaggle: web
  triggers: [{type: manual}]
  start: check
  tasks:
    - name: check
      type: deterministic
      goal: Run the local check
      run:
        command: ["true"]
      next: ""
`

type journalChildLauncher struct {
	cancelErr       error
	resultJournal   string
	resultDigest    string
	authority       childworkflow.Authority
	layout          instance.Layout
	starts, cancels int
	beforeStart     func(childExecutionStart) error
	prepareErr      error
	result          childExecutionResult
}

func (l *journalChildLauncher) Prepare(context.Context, childworkflow.ChildStartEnvelope) (childworkflow.Authority, error) {
	return l.authority, l.prepareErr
}
func (l *journalChildLauncher) Start(_ context.Context, start childExecutionStart) error {
	l.starts++
	if l.beforeStart != nil {
		if err := l.beforeStart(start); err != nil {
			return err
		}
	}
	return l.publish(start)
}
func (l *journalChildLauncher) publish(start childExecutionStart) error {
	e := start.Envelope
	run, err := journal.Create(l.layout.ForGaggle(e.Gaggle).RunsDir(), journal.RunIdentity{
		RunID: start.Child.RunID, Workflow: e.Workflow, WorkflowVersion: start.Proposal.Machine.Def.Version, WorkflowDigest: e.WorkflowDigest, GooberDigest: e.ParentGooberDigest, ConfigGeneration: e.ConfigGeneration, Gaggle: e.Gaggle, Child: &start.Lineage, Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		return err
	}
	return run.Close()
}
func (l *journalChildLauncher) Cancel(context.Context, childExecutionRef) error {
	l.cancels++
	return l.cancelErr
}
func (l *journalChildLauncher) Result(context.Context, childExecutionRef) (childExecutionResult, error) {
	if l.resultDigest != "" {
		reader, err := journal.OpenReadOnly(l.resultJournal)
		if err != nil {
			return childExecutionResult{}, err
		}
		raw, err := reader.ArtifactByDigest(l.resultDigest)
		if err != nil {
			return childExecutionResult{}, err
		}
		var result childExecutionResult
		if err = json.Unmarshal(raw, &result); err != nil {
			return childExecutionResult{}, err
		}
		result.ResultRef = l.result.ResultRef
		return result, nil
	}
	return l.result, nil
}

type childDrainFixture struct {
	service    *durableTriggerService
	launcher   *journalChildLauncher
	submission childworkflow.Submission
	path       string
}

func newChildDrainFixture(t *testing.T, customSource ...string) childDrainFixture {
	source := dispatchChildSource
	if len(customSource) > 0 {
		source = customSource[0]
	}
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	path := filepath.Join(layout.Root, "accepted.db")
	dispatch := newDaemonTriggerService()
	s := acceptedService(t, path, dispatch)
	s.observeChild = acceptedChildObserver(layout)
	input := childworkflow.AdmissionContext{
		Config: &instance.Config{}, Gaggle: apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: "github"}}},
		ParentTask:   apiv1.Task{Name: "plan", Type: apiv1.TaskAgentic, ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}, AllowedCapabilities: []string{"agent:model", "repo:read"}}},
		ConfigDigest: journal.Digest([]byte("config")), GrantedCapabilities: []string{"agent:model", "repo:read"},
		Goobers: map[string]apiv1.GooberSpec{"coder": {Gaggle: "web", Harness: "fake", Capabilities: []string{"agent:model", "repo:read"}}}, KnownChecks: []string{"status-equals"}, KnownHarnesses: []string{"fake"}, Backend: childworkflow.BackendRunner,
	}
	validator, err := childworkflow.NewValidator(input)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := validator.Validate([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	authority := childworkflow.Authority{Origin: childworkflow.Origin{GrantID: "grant", Gaggle: "web", RunID: strings.Repeat("a", 32), StageOccurrence: "plan/branch0/visit1", AttemptID: "attempt-1", ConfigDigest: input.ConfigDigest, PolicyDigest: proposal.PolicyDigest}, Actor: childworkflow.InvocationActor(strings.Repeat("a", 32), "plan/branch0/visit1"), Admission: input, ConfigGeneration: journal.Digest([]byte("archive")), ParentWorkflow: "parent", ParentWorkflowDigest: journal.Digest([]byte("parent")), ParentGooberDigest: journal.Digest([]byte("goobers"))}
	launcher := &journalChildLauncher{authority: authority, layout: layout}
	s.children = launcher
	submissionService := &childworkflow.SubmissionService{Queue: s.queue, Authority: childworkflow.AuthorityResolverFunc(func(context.Context, childworkflow.Origin) (childworkflow.Authority, error) { return authority, nil })}
	now := time.Now()
	if err = s.queue.BindChildAuthority(t.Context(), authority.Origin.Binding(now.Add(time.Hour)), "", now); err != nil {
		t.Fatal(err)
	}
	submission, err := submissionService.Submit(t.Context(), authority.Origin, childworkflow.SubmissionRequest{InvocationKey: "first-child", Source: []byte(source)})
	if err != nil {
		t.Fatal(err)
	}
	return childDrainFixture{service: s, launcher: launcher, submission: submission, path: path}
}
func (f *childDrainFixture) state(t *testing.T) (triggerqueue.ChildRecord, triggerqueue.Record) {
	t.Helper()
	c, err := f.service.queue.GetChild(t.Context(), f.submission.Child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.service.queue.ChildStart(t.Context(), c.Identity)
	if err != nil {
		t.Fatal(err)
	}
	return c, r
}
func (f *childDrainFixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.service.queue.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = acceptedService(t, f.path, newDaemonTriggerService())
	f.service.children = f.launcher
	f.service.observeChild = acceptedChildObserver(f.launcher.layout)
}
func drainChildTwice(t *testing.T, f *childDrainFixture) {
	t.Helper()
	for range 2 {
		if err := f.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChildDrainLostHandoffRecoversExactJournalWithoutSecondStart(t *testing.T) {
	f := newChildDrainFixture(t)
	f.launcher.beforeStart = func(start childExecutionStart) error {
		if string(start.Proposal.Source) != dispatchChildSource || start.Child.RunID != f.submission.Child.RunID || start.Lineage.SourceDigest != f.submission.Envelope.SourceDigest {
			t.Fatal("launch changed source or stable identity")
		}
		if err := f.launcher.publish(start); err != nil {
			return err
		}
		return errors.New("response lost after journal publication")
	}
	if err := f.service.Drain(t.Context()); err == nil {
		t.Fatal("expected uncertain handoff")
	}
	_, receipt := f.state(t)
	if receipt.State != triggerqueue.Dispatching {
		t.Fatal("ambiguous start lost claim")
	}
	f.reopen(t)
	drainChildTwice(t, &f)
	child, receipt := f.state(t)
	if child.State != triggerqueue.ChildRunning || receipt.State != triggerqueue.Dispatched || f.launcher.starts != 1 {
		t.Fatalf("reconciled child=%s receipt=%s starts=%d", child.State, receipt.State, f.launcher.starts)
	}
}

func TestChildDrainRetriesOnlyKnownUnstartedAdmission(t *testing.T) {
	f := newChildDrainFixture(t)
	f.launcher.beforeStart = func(childExecutionStart) error { return &childStartDeferred{Reason: "capacity"} }
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, receipt := f.state(t)
	if receipt.State != triggerqueue.Accepted {
		t.Fatal("temporary capacity refusal lost custody")
	}
	f.launcher.beforeStart = nil
	drainChildTwice(t, &f)
	c, receipt := f.state(t)
	if f.launcher.starts != 2 || c.State != triggerqueue.ChildRunning || receipt.RunID != f.submission.Child.RunID {
		t.Fatalf("retry=%+v starts=%d", c, f.launcher.starts)
	}
}

func TestChildDrainRestartRetriesAbsentClaimOnlyOnce(t *testing.T) {
	f := newChildDrainFixture(t)
	if err := f.service.queue.BeginDispatch(t.Context(), f.submission.Child.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	// The fake handoff produces no journal. Current-process uncertainty must
	// not be replayed, even though boot-time strong absence permitted one retry.
	f.launcher.beforeStart = func(childExecutionStart) error { return errors.New("handoff outcome unknown") }
	if err := f.service.Drain(t.Context()); err == nil {
		t.Fatal("expected handoff error")
	}
	for range 3 {
		if err := f.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	_, r := f.state(t)
	if f.launcher.starts != 1 || r.State != triggerqueue.Dispatching {
		t.Fatalf("replayed unknown start: %d %s", f.launcher.starts, r.State)
	}
}

func TestChildDrainMismatchDoesNotReleaseUncertainClaim(t *testing.T) {
	f := newChildDrainFixture(t)
	f.launcher.beforeStart = func(start childExecutionStart) error {
		start.Lineage.SourceDigest = journal.Digest([]byte("other"))
		if err := f.launcher.publish(start); err != nil {
			return err
		}
		return errors.New("lost response")
	}
	if err := f.service.Drain(t.Context()); err == nil {
		t.Fatal("expected handoff error")
	}
	f.reopen(t)
	if err := f.service.Drain(t.Context()); err == nil {
		t.Fatal("mismatched journal accepted")
	}
	_, r := f.state(t)
	if f.launcher.starts != 1 || r.State != triggerqueue.Dispatching {
		t.Fatalf("mismatched child replayed: %d %s", f.launcher.starts, r.State)
	}
}

func TestChildDrainRefusesRevocationSourceTamperAndPinsBeforeClaim(t *testing.T) {
	for _, mode := range []string{"revoked", "tampered", "missing", "changed-policy", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			f := newChildDrainFixture(t)
			switch mode {
			case "revoked":
				f.launcher.prepareErr = childworkflow.ErrAuthorityUnavailable
			case "changed-policy":
				f.launcher.authority.Admission.ParentTask.ChildWorkflows.AllowPRPublication = true
			default:
				db, err := sql.Open("sqlite", f.path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				query := `UPDATE child_proposals SET source='tampered'`
				if mode == "missing" {
					query = `DELETE FROM child_proposals`
				}
				if mode == "hybrid" {
					_, r := f.state(t)
					var doc map[string]any
					if err = json.Unmarshal(r.Payload, &doc); err != nil {
						t.Fatal(err)
					}
					doc["request"] = map[string]any{"workflow": "catalog"}
					raw, e := json.Marshal(doc)
					if e != nil {
						t.Fatal(e)
					}
					_, err = db.Exec(`UPDATE triggers SET payload=?`, raw)
				} else {
					_, err = db.Exec(query)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.Drain(t.Context()); err == nil {
				t.Fatal("invalid execution accepted")
			}
			_, r := f.state(t)
			if f.launcher.starts != 0 || r.State != triggerqueue.Accepted {
				t.Fatalf("refusal claimed child: %d %s", f.launcher.starts, r.State)
			}
		})
	}
}

func TestChildDrainCancellationRequiresObservedResult(t *testing.T) {
	f := newChildDrainFixture(t)
	drainChildTwice(t, &f)
	if err := f.service.queue.FenceChildParent(t.Context(), f.submission.Child.Identity.ChildParent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	// A successful Cancel delivery is not terminal proof.
	drainChildTwice(t, &f)
	c, _ := f.state(t)
	if c.State != triggerqueue.ChildRunning || f.launcher.cancels == 0 {
		t.Fatalf("cancel falsely terminalized: %+v", c)
	}
	persistChildResult(t, &f, triggerqueue.ChildCancelled)
	drainChildTwice(t, &f)
	c, _ = f.state(t)
	if c.State != triggerqueue.ChildCancelled || c.ResultRef != f.launcher.result.ResultRef || !c.AcknowledgedAt.IsZero() {
		t.Fatalf("cancel result=%+v", c)
	}
	cancels := f.launcher.cancels
	drainChildTwice(t, &f)
	if f.launcher.cancels != cancels {
		t.Fatal("terminal child stayed in cancel outbox")
	}
}

func TestChildDrainQueuedCancellationNeverStarts(t *testing.T) {
	f := newChildDrainFixture(t)
	if err := f.service.queue.FenceChildParent(t.Context(), f.submission.Child.Identity.ChildParent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	f.launcher.result = childExecutionResult{State: triggerqueue.ChildCancelled, ResultRef: "verified-result:unstarted"}
	drainChildTwice(t, &f)
	c, r := f.state(t)
	if c.State != triggerqueue.ChildCancelled || r.State != triggerqueue.Rejected || f.launcher.starts != 0 {
		t.Fatalf("cancelled queue started: %+v %+v", c, r)
	}
}

func TestGeneratedChildCannotUseCatalogGenerationRecovery(t *testing.T) {
	// Refusal precedes even the first generation-store read, preventing a child
	// display-name collision from accidentally selecting a catalog machine.
	resolver := generationResolverFor(instance.Layout{}, &configgeneration.Retainer{}, nil)
	if _, err := resolver(t.Context(), journal.RunIdentity{Child: &journal.ChildLineage{}}); err == nil {
		t.Fatal("generated child used ordinary generation recovery")
	}
}

// The result adapter's test implementation verifies a real immutable journal
// artifact before returning custody, rather than inventing a terminal receipt.
func persistChildResult(t *testing.T, f *childDrainFixture, state triggerqueue.ChildState) {
	t.Helper()
	dir := filepath.Join(f.launcher.layout.ForGaggle("web").RunsDir(), f.submission.Child.RunID)
	run, _, err := journal.Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	result := childExecutionResult{State: state, WorkspaceRef: "verified-workspace:fork"}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := run.RecordArtifact("child-result.json", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	f.launcher.resultJournal = dir
	f.launcher.resultDigest = ref.Digest
	result.ResultRef = "journal:" + f.submission.Child.RunID + ":" + ref.Digest
	f.launcher.result = result
}

func TestChildDrainCancelFailureDoesNotHideDurableCompletion(t *testing.T) {
	f := newChildDrainFixture(t)
	drainChildTwice(t, &f)
	if err := f.service.queue.FenceChildParent(t.Context(), f.submission.Child.Identity.ChildParent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	f.launcher.cancelErr = errors.New("cancel delivery unavailable")
	persistChildResult(t, &f, triggerqueue.ChildCompleted)
	// The sweep cursor may require a wrap, but cancel failure remains visible
	// while independently verified completion is durably reconciled.
	var failures error
	for range 2 {
		failures = errors.Join(failures, f.service.Drain(t.Context()))
	}
	c, _ := f.state(t)
	if failures == nil || c.State != triggerqueue.ChildCompleted {
		t.Fatalf("cancel masked completion: %s %v", c.State, failures)
	}
}
