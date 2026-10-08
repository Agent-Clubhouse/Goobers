package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimability"
	"github.com/goobers/goobers/internal/fleetdiagnostics"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

func TestBacklogPendingAgeCoveragePauseReloadAndProgress(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	counter := &backlogCounter{observation: backlogPollObservation{observedAt: now, count: 2, complete: true}}
	identity := localscheduler.WorkflowIdentity{Gaggle: "g", Workflow: "issues"}
	sources := map[localscheduler.WorkflowIdentity]backlogObservationReader{identity: counter}
	sampler := &backlogHealthSampler{ages: map[localscheduler.WorkflowIdentity]backlogAge{}}
	observe := func(at time.Time, state string, progress time.Time) map[string]any {
		attrs := map[string]any{"state": state, "lastUsefulProgressAt": progress.Format(time.RFC3339Nano)}
		retained := map[localscheduler.WorkflowIdentity]backlogAge{}
		sampler.observe(t.Context(), attrs, "g", at, time.Minute, sources, retained)
		sampler.ages = retained
		return attrs
	}
	if got := observe(now, "unknown", time.Time{}); got["backlogState"] != "pending" || got["backlogPendingCount"] != 2 {
		t.Fatal(got)
	}
	counter.observation.observedAt = now.Add(time.Minute)
	if got := observe(now.Add(time.Minute), "unknown", time.Time{}); got["backlogState"] != "attention" {
		t.Fatal(got)
	}
	if got := observe(now.Add(time.Minute), "unknown", now.Add(time.Minute)); got["backlogState"] != "pending" {
		t.Fatal("useful progress did not reset age", got)
	}
	observe(now.Add(time.Minute), "paused", time.Time{})
	if len(sampler.ages) != 0 {
		t.Fatal("pause retained age")
	}
	counter.observation.complete = false
	if got := observe(now.Add(time.Minute), "unknown", time.Time{}); got["backlogCoverage"] != "partial" || got["backlogState"] != "pending" {
		t.Fatal(got)
	}
	if len(sampler.ages) != 0 {
		t.Fatal("partial coverage retained age")
	}
	counter.observation.count = 0
	if got := observe(now.Add(time.Minute), "unknown", time.Time{}); got["backlogPendingCount"] != nil {
		t.Fatal("partial zero", got)
	}
	counter.observation.complete = true
	if got := observe(now.Add(time.Minute), "unknown", time.Time{}); got["backlogState"] != "empty" {
		t.Fatal(got)
	}
	if got := observe(now.Add(3*time.Minute), "unknown", time.Time{}); got["backlogPendingCount"] != nil {
		t.Fatal("stale count", got)
	}
	counter.observation = backlogPollObservation{observedAt: now.Add(3 * time.Minute), count: 1, complete: true}
	observe(now.Add(3*time.Minute), "unknown", time.Time{})
	replacement := &backlogCounter{observation: backlogPollObservation{observedAt: now.Add(4 * time.Minute), count: 1, complete: true}}
	sources[identity] = replacement
	if got := observe(now.Add(4*time.Minute), "unknown", time.Time{}); got["backlogState"] != "pending" {
		t.Fatal("new generation inherited age", got)
	}
}

func TestBacklogFailedPollDoesNotBecomeEmpty(t *testing.T) {
	counter := &backlogCounter{}
	counter.retainBacklogObservation(backlogPollObservation{complete: true}, 0, errors.New("provider unavailable"))
	got := counter.backlogObservation()
	if !got.failed || got.complete || got.observedAt.IsZero() {
		t.Fatal(got)
	}
}

func TestBacklogObservationStrictWireAndOverlappingSelectors(t *testing.T) {
	now := time.Now().UTC()
	source := &backlogCounter{observation: backlogPollObservation{observedAt: now, count: 2, complete: true}}
	sources := map[localscheduler.WorkflowIdentity]backlogObservationReader{{Gaggle: "g", Workflow: "a"}: source, {Gaggle: "g", Workflow: "b"}: source}
	sampler := &backlogHealthSampler{ages: map[localscheduler.WorkflowIdentity]backlogAge{}}
	attrs := map[string]any{"schemaVersion": 1, "deploymentId": "d", "instanceId": "i", "gaggleId": "g", "component": "daemon", "bootId": "b", "bootStartedAt": now.Format(time.RFC3339Nano), "sequence": 1, "observedAt": now.Format(time.RFC3339Nano), "windowStart": now.Format(time.RFC3339Nano), "windowCoverage": "partial", "state": "unknown", "reasonCode": "work_eligibility_unknown"}
	sampler.observe(t.Context(), attrs, "g", now, time.Minute, sources, map[localscheduler.WorkflowIdentity]backlogAge{})
	h, err := fleetdiagnostics.DecodeHeartbeat(attrs)
	if err != nil || h.Backlog == nil || h.Backlog.PendingCount == nil || *h.Backlog.PendingCount != 2 {
		t.Fatalf("%+v %v", h.Backlog, err)
	}
	if h.EligibleCount != nil {
		t.Fatal("pending work asserted claimability")
	}
}

func claimabilityHeartbeat(now time.Time) map[string]any {
	return map[string]any{"schemaVersion": 1, "deploymentId": "d", "instanceId": "i", "gaggleId": "g", "component": "daemon", "bootId": "b", "bootStartedAt": now.Format(time.RFC3339Nano), "sequence": 1, "observedAt": now.Format(time.RFC3339Nano), "windowStart": now.Format(time.RFC3339Nano), "windowCoverage": "partial", "state": "unknown", "reasonCode": "work_eligibility_unknown"}
}

func TestBacklogClaimabilityEvidenceIntegratesIntoFleetHealth(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	target := &backlogClaimTarget{policy: claimability.Policy{Gaggle: "g", Provider: "github"}, window: time.Hour}
	counter := &backlogCounter{claim: target, observation: backlogPollObservation{observedAt: now, count: 2, complete: true, candidates: []claimability.Candidate{{ID: "1"}, {ID: "2"}}}}
	identity := localscheduler.WorkflowIdentity{Gaggle: "g", Workflow: "issues"}
	sources := map[localscheduler.WorkflowIdentity]backlogObservationReader{identity: counter}
	probes := 0
	result := claimability.Result{Observed: 2, Available: 1, Held: 1, Complete: true}
	sampler := &backlogHealthSampler{ages: map[localscheduler.WorkflowIdentity]backlogAge{}, probe: func(_ context.Context, _ time.Time, got backlogClaimTarget, evidence backlogPollObservation) claimability.Result {
		probes++
		if got != *target || len(evidence.candidates) != evidence.count {
			t.Fatalf("probe target %+v evidence %+v", got, evidence)
		}
		return result
	}}
	observe := func(at time.Time, state string) map[string]any {
		attrs := claimabilityHeartbeat(at)
		attrs["state"] = state
		retained := map[localscheduler.WorkflowIdentity]backlogAge{}
		sampler.observe(t.Context(), attrs, "g", at, time.Minute, sources, retained)
		sampler.ages = retained
		sampler.pruneClaims(sources)
		if _, err := fleetdiagnostics.DecodeHeartbeat(attrs); err != nil {
			t.Fatalf("wire rejected %v: %v", attrs, err)
		}
		return attrs
	}
	if got := observe(now, "unknown"); got["backlogReasonCode"] != "claimable_observed" || got["backlogClaimableCount"] != 1 {
		t.Fatal(got)
	}
	observe(now, "unknown")
	if probes != 1 {
		t.Fatalf("one poll was classified %d times", probes)
	}
	counter.observation.observedAt = now.Add(time.Minute)
	if got := observe(now.Add(time.Minute), "unknown"); got["backlogState"] != "attention" || got["backlogReasonCode"] != "claimable_without_confirmed_progress" || got["backlogClaimableCount"] != 1 {
		t.Fatal(got)
	}
	// Every pending item held or waiting is deferral, not a stall: no age.
	result = claimability.Result{Observed: 2, Held: 1, Waiting: 1, Complete: true}
	counter.observation.observedAt = now.Add(2 * time.Minute)
	if got := observe(now.Add(2*time.Minute), "unknown"); got["backlogState"] != "pending" || got["backlogReasonCode"] != "pending_held" || got["backlogClaimableCount"] != 0 || len(sampler.ages) != 0 {
		t.Fatal(got, sampler.ages)
	}
	// Held evidence over a partial poll is not proof; it ages as unknown.
	counter.observation = backlogPollObservation{observedAt: now.Add(3 * time.Minute), count: 2, complete: false, candidates: counter.observation.candidates}
	if got := observe(now.Add(3*time.Minute), "unknown"); got["backlogReasonCode"] != "claimability_unknown" || got["backlogClaimableCount"] != nil {
		t.Fatal(got)
	}
	result = claimability.Result{Observed: 2, Available: 1, Unknown: 1}
	counter.observation = backlogPollObservation{observedAt: now.Add(4 * time.Minute), count: 2, complete: true, candidates: counter.observation.candidates}
	before := probes
	if got := observe(now.Add(4*time.Minute), "paused"); got["backlogReasonCode"] != "operator_paused" || got["backlogClaimableCount"] != nil || probes != before {
		t.Fatal("paused work was classified", got)
	}
	// A reloaded counter without a derivable admission identity stays unknown.
	sources[identity] = &backlogCounter{observation: backlogPollObservation{observedAt: now.Add(4 * time.Minute), count: 2, complete: true}}
	if got := observe(now.Add(4*time.Minute), "unknown"); got["backlogReasonCode"] != "claimability_unknown" || len(sampler.claims) != 0 {
		t.Fatal(got)
	}
}

func TestBacklogClaimTargetMirrorsWorkflowAdmission(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "o", Name: "r"}
	workflow := func(visibility, lease string) *apiv1.Workflow {
		wf := &apiv1.Workflow{}
		wf.Spec.Gaggle, wf.Spec.Readiness.ClaimVisibility = "g", visibility
		wf.Spec.Tasks = []apiv1.Task{{Name: "claim", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query"}}, Inputs: map[string]string{"leaseDuration": lease}}}
		return wf
	}
	if got := newBacklogClaimTarget(workflow("shared", "2h"), repo, apiv1.ProviderGitHub); got == nil || !got.policy.Shared || got.window != 2*time.Hour || got.maxWindow != 2*time.Hour || got.policy.Gaggle != "g" || got.policy.Provider != "github" {
		t.Fatalf("shared target %+v", got)
	}
	if got := newBacklogClaimTarget(workflow("", ""), repo, ""); got == nil || got.policy.Shared || got.window != DefaultClaimLease {
		t.Fatalf("local target %+v", got)
	}
	for name, wf := range map[string]*apiv1.Workflow{"unknown visibility": workflow("other", ""), "templated lease": workflow("", "${lease}")} {
		if newBacklogClaimTarget(wf, repo, "") != nil {
			t.Errorf("%s derived an admission identity", name)
		}
	}
	ado := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "o", Name: "r"}
	if newBacklogClaimTarget(workflow("shared", ""), ado, "") != nil {
		t.Error("unsupported shared provider derived an admission identity")
	}
	// Admission keys a cross-provider backlog's claims on the backlog provider.
	if newBacklogClaimTarget(workflow("", ""), ado, apiv1.ProviderGitHub) != nil {
		t.Error("cross-provider backlog derived the project provider's namespace")
	}
	// Several claim tasks: waiting needs the shortest lease, available the longest.
	wf := workflow("", "5m")
	wf.Spec.Tasks = append(wf.Spec.Tasks, apiv1.Task{Name: "reclaim", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query"}}})
	if got := newBacklogClaimTarget(wf, repo, ""); got == nil || got.window != 5*time.Minute || got.maxWindow != DefaultClaimLease {
		t.Fatalf("mixed lease target %+v", got)
	}
	wf = workflow("", "")
	wf.Spec.Tasks[0].Run.Command = []string{"goobers", "select-source"}
	if newBacklogClaimTarget(wf, repo, "") != nil {
		t.Error("non backlog-query admission derived an identity")
	}
}

// TestDaemonBacklogClaimProbeReadsRealClaimPlaneReadOnly seeds the real local
// claim ledger and blocked records, observes through the daemon probe, proves
// neither file changed, and checks the claim stage's own ledger admission
// agrees with every definitive verdict.
func TestDaemonBacklogClaimProbeReadsRealClaimPlaneReadOnly(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "o", Name: "r"}
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(layout.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	key := func(id string) localscheduler.ClaimKey {
		return localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: id}
	}
	if ok, _, err := ledger.ClaimScoped(key("held"), "other-run", "issues", time.Hour); err != nil || !ok {
		t.Fatalf("seed lease: %v %v", ok, err)
	}
	store, err := heldStateStore(layout)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := encodeBlockedRecords(map[string]blockedRecord{blockedRecordKey(repo, "blocked"): {Repository: repo, ItemID: "blocked", Blockers: []string{"1"}, RunID: "r", RecordedAt: time.Now().UTC()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(t.Context(), stateclient.KeyBlockedRecords, blocked, ""); err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string][]byte {
		files := map[string][]byte{}
		entries, err := os.ReadDir(layout.SchedulerDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(layout.SchedulerDir(), entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files[entry.Name()] = data
		}
		return files
	}
	before := snapshot()
	target := backlogClaimTarget{policy: claimability.Policy{Gaggle: "g", Provider: "github"}, window: DefaultClaimLease, repo: repo}
	probe := daemonBacklogClaimProbe(&schedulerSetup{Root: root})
	verdict := func(id string) claimability.Result {
		return probe(t.Context(), time.Now().UTC(), target, backlogPollObservation{count: 1, complete: true, candidates: []claimability.Candidate{{ID: id}}})
	}
	// A learned block is re-checked by admission each cycle, so it only
	// proves ambiguity.
	free, held, blockedItem := verdict("free"), verdict("held"), verdict("blocked")
	if free.Available != 1 || held.Held != 1 || blockedItem.Unknown != 1 || !free.Complete || !held.Complete || blockedItem.Complete {
		t.Fatalf("free %+v held %+v blocked %+v", free, held, blockedItem)
	}
	shared := target
	shared.policy.Shared = true
	if got := probe(t.Context(), time.Now().UTC(), shared, backlogPollObservation{count: 1, complete: true, candidates: []claimability.Candidate{{ID: "free"}}}); got.Unknown != 1 || got.Complete {
		t.Fatalf("shared without quota or credentials: %+v", got)
	}
	after := snapshot()
	if len(after) != len(before) {
		t.Fatalf("observation created scheduler files: %d -> %d", len(before), len(after))
	}
	for name, data := range before {
		if !bytes.Equal(data, after[name]) {
			t.Fatalf("observation changed %s", name)
		}
	}
	if ok, _, err := ledger.ClaimScoped(key("free"), "this-run", "issues", time.Hour); err != nil || !ok {
		t.Fatalf("admission refused verified-available item: %v %v", ok, err)
	}
	if ok, _, err := ledger.ClaimScoped(key("held"), "this-run", "issues", time.Hour); err != nil || ok {
		t.Fatalf("admission accepted observed-held item: %v %v", ok, err)
	}
}

func TestBacklogPollRetainsBoundedCandidatesAndDropsThemOnFailure(t *testing.T) {
	var observation backlogPollObservation
	for i := range backlogClaimCandidateLimit + 1 {
		item := providers.WorkItem{ID: fmt.Sprint(i)}
		if i == 0 {
			item.Labels = []string{providers.LabelClaimed}
		}
		observation.retainCandidate(item)
	}
	if len(observation.candidates) != backlogClaimCandidateLimit || !observation.truncated || !observation.candidates[0].ProviderClaimed || observation.candidates[1].ProviderClaimed {
		t.Fatalf("%+v", observation)
	}
	counter := &backlogCounter{}
	counter.retainBacklogObservation(observation, 0, errors.New("provider unavailable"))
	if got := counter.backlogObservation(); got.candidates != nil || got.truncated {
		t.Fatal("failed poll retained candidates", got)
	}
}
