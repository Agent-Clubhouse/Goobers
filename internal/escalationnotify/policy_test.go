package escalationnotify

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

var webRepo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

var webRepoRef = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}

// fakePoster records every provider write the policy makes.
type fakePoster struct {
	updates   []providers.UpdateWorkItemRequest
	comments  []providers.Comment
	edits     []string
	updateErr error
	listErr   error
	ctxs      []context.Context
}

var _ gate.Commenter = (*fakePoster)(nil)

func (f *fakePoster) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return f.comments, f.listErr
}

func (f *fakePoster) UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	f.updates = append(f.updates, req)
	f.ctxs = append(f.ctxs, ctx)
	return providers.WorkItem{}, f.updateErr
}

func (f *fakePoster) UpdateComment(_ context.Context, _ providers.RepositoryRef, commentID, _ string) error {
	f.edits = append(f.edits, commentID)
	return nil
}

func (f *fakePoster) labelUpdates() []providers.UpdateWorkItemRequest {
	var out []providers.UpdateWorkItemRequest
	for _, u := range f.updates {
		if len(u.AddLabels) > 0 || len(u.RemoveLabels) > 0 {
			out = append(out, u)
		}
	}
	return out
}

// fakeState is an in-memory State.
type fakeState struct {
	claimedIDs   map[string][]string
	claimedErr   error
	items        map[string][]Item
	itemsErr     error
	backlog      func(providers.RepositoryRef) providers.RepositoryRef
	blocks       []Block
	cycle        blockedcycle.Result
	recordErr    error
	streaks      map[string]int
	loadErr      error
	writeErr     error
	reconcileErr error
	reconciles   int
	parkFailures []string
	cleared      []string
	voided       []string
	settled      []string
	runURLs      []string
}

func newFakeState() *fakeState {
	return &fakeState{
		claimedIDs: map[string][]string{},
		items:      map[string][]Item{},
		streaks:    map[string]int{},
	}
}

func (s *fakeState) ClaimedItemIDs(runID string) ([]string, error) {
	return s.claimedIDs[runID], s.claimedErr
}

func (s *fakeState) ClaimedItems(runID string) ([]Item, error) {
	return s.items[runID], s.itemsErr
}

func (s *fakeState) BacklogRepository(repo providers.RepositoryRef) providers.RepositoryRef {
	if s.backlog != nil {
		return s.backlog(repo)
	}
	return repo
}

func (s *fakeState) RecordBlock(b Block) (blockedcycle.Result, error) {
	s.blocks = append(s.blocks, b)
	return s.cycle, s.recordErr
}

func (s *fakeState) LoadFailureStreak(_ context.Context, _ gate.Commenter, repo providers.RepositoryRef, itemID string) (int, error) {
	return s.streaks[repo.Name+"#"+itemID], s.loadErr
}

func (s *fakeState) WriteFailureStreak(repo providers.RepositoryRef, itemID string, count int, _, _ string) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.streaks[repo.Name+"#"+itemID] = count
	return nil
}

func (s *fakeState) ReconcileParkOutbox(context.Context, gate.Commenter) error {
	s.reconciles++
	return s.reconcileErr
}

func (s *fakeState) RecordParkFailure(repo providers.RepositoryRef, itemID, _, _ string, _ int, _ error) error {
	s.parkFailures = append(s.parkFailures, repo.Name+"#"+itemID)
	return nil
}

func (s *fakeState) ClearParks(repo providers.RepositoryRef, itemID string) error {
	s.cleared = append(s.cleared, repo.Name+"#"+itemID)
	return nil
}

func (s *fakeState) VoidRemediationCharge(_ context.Context, _ gate.Commenter, runID string) error {
	s.voided = append(s.voided, runID)
	return nil
}

func (s *fakeState) SettleNoWorkStreak(_ context.Context, _ gate.Commenter, runID, _, _ string) error {
	s.settled = append(s.settled, runID)
	return nil
}

func (s *fakeState) RunURL(runID string) (string, error) {
	s.runURLs = append(s.runURLs, runID)
	return "http://portal/#/run/" + runID, nil
}

func newTestPolicy(t *testing.T) (*Policy, *fakePoster, *fakeState) {
	t.Helper()
	poster := &fakePoster{}
	state := newFakeState()
	return &Policy{Poster: poster, RunsDir: t.TempDir(), State: state}, poster, state
}

func TestBlockedNamedBlockerParksBlockedOnSiblingAndRecords(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	err := p.Blocked(context.Background(), runner.BlockedOutcome{
		RunID: "run-1", RepoRef: webRepoRef, Stage: "implement", ItemID: "42",
		Reason: "waits on #41", Blockers: []string{"41"},
	})
	if err != nil {
		t.Fatalf("Blocked: %v", err)
	}
	if len(state.blocks) != 1 || state.blocks[0].ItemID != "42" || !slices.Equal(state.blocks[0].Blockers, []string{"41"}) ||
		state.blocks[0].RunID != "run-1" || state.blocks[0].Stage != "implement" || state.blocks[0].Reason != "waits on #41" {
		t.Fatalf("recorded blocks = %+v", state.blocks)
	}
	if len(poster.updates) != 1 {
		t.Fatalf("updates = %+v, want one park", poster.updates)
	}
	got := poster.updates[0]
	if got.ID != "42" || got.Repository != webRepo ||
		!slices.Equal(got.AddLabels, []string{providers.LabelBlockedOnSibling}) ||
		!slices.Equal(got.RemoveLabels, []string{providers.LabelReady, providers.LabelClaimed}) {
		t.Fatalf("park request = %+v", got)
	}
}

func TestBlockedUnattributedAndSelfOnlyBlockParkNeedsHumanWithoutRecord(t *testing.T) {
	for name, blockers := range map[string][]string{"none": nil, "self only": {"42"}} {
		t.Run(name, func(t *testing.T) {
			p, poster, state := newTestPolicy(t)
			if err := p.Blocked(context.Background(), runner.BlockedOutcome{
				RunID: "run-1", RepoRef: webRepoRef, ItemID: "42", Blockers: blockers,
			}); err != nil {
				t.Fatalf("Blocked: %v", err)
			}
			if len(state.blocks) != 0 {
				t.Fatalf("recorded blocks = %+v, want none", state.blocks)
			}
			if len(poster.updates) != 1 || !slices.Equal(poster.updates[0].AddLabels, []string{providers.LabelNeedsHuman}) {
				t.Fatalf("updates = %+v, want one needs-human park", poster.updates)
			}
		})
	}
}

func TestBlockedResolvesClaimedItemsAndScopesToBacklog(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	backlog := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "backlog"}
	state.backlog = func(providers.RepositoryRef) providers.RepositoryRef { return backlog }
	state.claimedIDs["run-1"] = []string{"7", "8"}
	if err := p.Blocked(context.Background(), runner.BlockedOutcome{
		RunID: "run-1", RepoRef: webRepoRef, Blockers: []string{"7", "9"},
	}); err != nil {
		t.Fatalf("Blocked: %v", err)
	}
	if len(state.blocks) != 2 {
		t.Fatalf("recorded blocks = %+v, want one per claimed item", state.blocks)
	}
	// Item 7 named itself; only its real blocker survives.
	if !slices.Equal(state.blocks[0].Blockers, []string{"9"}) || state.blocks[0].Repository != backlog {
		t.Fatalf("item 7 block = %+v", state.blocks[0])
	}
	if len(poster.updates) != 2 || poster.updates[0].Repository != backlog || poster.updates[1].ID != "8" {
		t.Fatalf("updates = %+v", poster.updates)
	}
}

func TestBlockedNoClaimIsANoop(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	if err := p.Blocked(context.Background(), runner.BlockedOutcome{RunID: "run-1", RepoRef: webRepoRef}); err != nil {
		t.Fatalf("Blocked: %v", err)
	}
	if len(poster.updates) != 0 || len(state.blocks) != 0 {
		t.Fatalf("updates=%+v blocks=%+v, want none", poster.updates, state.blocks)
	}
	state.claimedErr = errors.New("ledger unreadable")
	if err := p.Blocked(context.Background(), runner.BlockedOutcome{RunID: "run-1", RepoRef: webRepoRef}); err == nil {
		t.Fatal("want the claim ledger error")
	}
}

func TestBlockedRequiresRepository(t *testing.T) {
	p, poster, _ := newTestPolicy(t)
	err := p.Blocked(context.Background(), runner.BlockedOutcome{RunID: "run-1", ItemID: "42"})
	if err == nil || !strings.Contains(err.Error(), "has no repository") {
		t.Fatalf("err = %v, want missing repository", err)
	}
	if len(poster.updates) != 0 {
		t.Fatalf("updates = %+v, want none", poster.updates)
	}
}

func TestBlockedCycleEscalatesEveryMemberInsteadOfParking(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	state.recordErr = errors.New("write failed after detection")
	state.cycle = blockedcycle.Result{
		Affected: []blockedcycle.Node{{Repository: webRepo, ItemID: "42"}, {Repository: webRepo, ItemID: "41"}},
		Paths:    [][]string{{"42", "41", "42"}},
	}
	poster.updateErr = errors.New("provider down")
	err := p.Blocked(context.Background(), runner.BlockedOutcome{
		RunID: "run-1", RepoRef: webRepoRef, ItemID: "42", Blockers: []string{"41"},
	})
	if err == nil || !strings.Contains(err.Error(), "record block for 42") || !strings.Contains(err.Error(), "escalate circular dependency") {
		t.Fatalf("err = %v, want record and escalation failures joined", err)
	}
	comments := blockedcycle.Comments(state.cycle)
	if len(poster.updates) != len(state.cycle.Affected)*len(comments) {
		t.Fatalf("updates = %d, want one per member per comment", len(poster.updates))
	}
	for _, u := range poster.updates {
		if u.Comment == "" || !slices.Equal(u.AddLabels, []string{providers.LabelNeedsHuman}) {
			t.Fatalf("cycle escalation = %+v", u)
		}
	}
}

func TestBlockedParkFailureIsReturned(t *testing.T) {
	p, poster, _ := newTestPolicy(t)
	poster.updateErr = errors.New("provider down")
	err := p.Blocked(context.Background(), runner.BlockedOutcome{RunID: "run-1", RepoRef: webRepoRef, ItemID: "42"})
	if err == nil || !strings.Contains(err.Error(), "park blocked item web#42") {
		t.Fatalf("err = %v, want park failure", err)
	}
}

// TestFailedSkipsInfraAndItemJudgmentDispositions is #3364: an infra-fault or
// item-judgment terminal never accrues a failure streak, even at threshold.
// Only an infra fault voids a charged remediation cycle (#5588).
func TestFailedSkipsInfraAndItemJudgmentDispositions(t *testing.T) {
	for _, tc := range []struct {
		code   string
		class  telemetry.ErrorClass
		voided bool
	}{
		{code: telemetry.ErrCodeCredentialUnavailable, voided: true},
		{code: telemetry.ErrCodeInfraGit, voided: true},
		{code: telemetry.ErrCodeIssueNotApplicable},
		// #5638: the runner's explicit class wins over the code.
		{code: telemetry.ErrCodeTimeout, class: telemetry.ErrorClassInfra, voided: true},
	} {
		t.Run(tc.code+"/"+string(tc.class), func(t *testing.T) {
			p, poster, state := newTestPolicy(t)
			state.items["run-1"] = []Item{{ItemID: "42", Repo: webRepo}}
			for i := 0; i < FailureStreakThreshold; i++ {
				if err := p.Failed(context.Background(), runner.FailedOutcome{
					RunID: "run-1", RepoRef: webRepoRef, Stage: "implement", Code: tc.code, FaultClass: tc.class,
				}); err != nil {
					t.Fatalf("Failed call %d: %v", i+1, err)
				}
			}
			if len(poster.updates) != 0 || len(state.streaks) != 0 {
				t.Fatalf("updates=%+v streaks=%+v, want none", poster.updates, state.streaks)
			}
			if got := len(state.voided) > 0; got != tc.voided {
				t.Fatalf("voided = %v, want %v", state.voided, tc.voided)
			}
		})
	}
}

func TestFailedCircuitBreakerTripsAtThreshold(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	state.items["run-1"] = []Item{{ItemID: "42", Repo: webRepo}}
	for i := 1; i <= FailureStreakThreshold; i++ {
		if err := p.Failed(context.Background(), runner.FailedOutcome{
			RunID: "run-1", RepoRef: webRepoRef, Stage: "implement", Code: telemetry.ErrCodeTimeout,
		}); err != nil {
			t.Fatalf("Failed call %d: %v", i, err)
		}
		if got := state.streaks["web#42"]; got != i {
			t.Fatalf("streak after call %d = %d", i, got)
		}
		if parks := poster.labelUpdates(); (i < FailureStreakThreshold) != (len(parks) == 0) {
			t.Fatalf("call %d: label updates = %+v", i, parks)
		}
	}
	park := poster.labelUpdates()[0]
	if !slices.Equal(park.AddLabels, []string{providers.LabelNeedsHuman}) || !slices.Equal(park.RemoveLabels, []string{providers.LabelReady}) {
		t.Fatalf("park = %+v", park)
	}
	if state.reconciles != FailureStreakThreshold || !slices.Equal(state.cleared, []string{"web#42"}) {
		t.Fatalf("reconciles=%d cleared=%v", state.reconciles, state.cleared)
	}
	if !slices.Contains(state.runURLs, "run-1") {
		t.Fatalf("run URL never resolved: %v", state.runURLs)
	}
}

func TestCircuitBreakerParkFailureGoesToOutbox(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	state.items["run-1"] = []Item{{ItemID: "42", Repo: webRepo}}
	state.streaks["web#42"] = FailureStreakThreshold - 1
	poster.updateErr = errors.New("provider down")
	err := p.Failed(context.Background(), runner.FailedOutcome{RunID: "run-1", RepoRef: webRepoRef, Code: "run_failed"})
	if err == nil || !strings.Contains(err.Error(), "apply circuit breaker on web#42") {
		t.Fatalf("err = %v, want park failure", err)
	}
	if !slices.Equal(state.parkFailures, []string{"web#42"}) || len(state.cleared) != 0 {
		t.Fatalf("parkFailures=%v cleared=%v", state.parkFailures, state.cleared)
	}
}

func TestCircuitBreakerStateFailures(t *testing.T) {
	t.Run("claimed items unknown", func(t *testing.T) {
		p, poster, state := newTestPolicy(t)
		state.reconcileErr = errors.New("outbox unreadable")
		state.itemsErr = errors.New("item repository unknown")
		err := p.Failed(context.Background(), runner.FailedOutcome{RunID: "run-1", Code: "run_failed"})
		if err == nil || !strings.Contains(err.Error(), "outbox unreadable") || !strings.Contains(err.Error(), "item repository unknown") {
			t.Fatalf("err = %v", err)
		}
		if len(poster.updates) != 0 {
			t.Fatalf("updates = %+v, want none", poster.updates)
		}
	})
	t.Run("streak unreadable", func(t *testing.T) {
		p, poster, state := newTestPolicy(t)
		state.items["run-1"] = []Item{{ItemID: "42", Repo: webRepo}}
		state.loadErr = errors.New("store down")
		err := p.Failed(context.Background(), runner.FailedOutcome{RunID: "run-1", Code: "run_failed"})
		if err == nil || !strings.Contains(err.Error(), "load failure streak state on web#42") || len(poster.updates) != 0 {
			t.Fatalf("err=%v updates=%+v", err, poster.updates)
		}
	})
	t.Run("streak unwritable", func(t *testing.T) {
		p, poster, state := newTestPolicy(t)
		state.items["run-1"] = []Item{{ItemID: "42", Repo: webRepo}}
		state.writeErr = errors.New("store down")
		err := p.Failed(context.Background(), runner.FailedOutcome{RunID: "run-1", Code: "run_failed"})
		if err == nil || !strings.Contains(err.Error(), "persist failure streak state on web#42") || len(poster.updates) != 0 {
			t.Fatalf("err=%v updates=%+v", err, poster.updates)
		}
	})
	t.Run("comment unwritable", func(t *testing.T) {
		p, poster, state := newTestPolicy(t)
		state.items["run-1"] = []Item{{ItemID: "42", Repo: webRepo}}
		poster.listErr = errors.New("rate limited")
		err := p.Failed(context.Background(), runner.FailedOutcome{RunID: "run-1", Code: "run_failed"})
		if err == nil || !strings.Contains(err.Error(), "upsert failure comment on web#42") || state.streaks["web#42"] != 1 {
			t.Fatalf("err=%v streaks=%v, want the count persisted despite the comment failure", err, state.streaks)
		}
	})
}

func TestTerminalNotifierRoutesByPhase(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	state.items["run-1"] = []Item{{ItemID: "42", Repo: webRepo}}
	state.streaks["web#42"] = 1
	var innerCalls []journal.RunPhase
	notify := p.TerminalNotifier(func(_ string, phase journal.RunPhase, _ string) error {
		innerCalls = append(innerCalls, phase)
		return nil
	})

	if err := notify("run-1", journal.PhaseFailed, "implement"); err != nil {
		t.Fatalf("failed phase: %v", err)
	}
	if state.streaks["web#42"] != 1 || len(poster.updates) != 0 {
		t.Fatalf("PhaseFailed must be left to Failed: streaks=%v updates=%+v", state.streaks, poster.updates)
	}
	if err := notify("run-1", journal.PhaseEscalated, "review"); err != nil {
		t.Fatalf("escalated phase: %v", err)
	}
	if state.streaks["web#42"] != 2 {
		t.Fatalf("escalated streak = %d, want 2", state.streaks["web#42"])
	}
	if err := notify("run-1", journal.PhaseCompleted, "done"); err != nil {
		t.Fatalf("completed phase: %v", err)
	}
	if state.streaks["web#42"] != 0 || !slices.Equal(state.settled, []string{"run-1"}) || !slices.Contains(state.cleared, "web#42") {
		t.Fatalf("completed: streaks=%v settled=%v cleared=%v", state.streaks, state.settled, state.cleared)
	}
	if !slices.Equal(innerCalls, []journal.RunPhase{journal.PhaseFailed, journal.PhaseEscalated, journal.PhaseCompleted}) {
		t.Fatalf("inner calls = %v", innerCalls)
	}
}

// labeledPoster is a fakePoster whose item carries labels, so it satisfies
// gate.WorkItemReader.
type labeledPoster struct {
	fakePoster
	labels []string
}

func (f *labeledPoster) GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error) {
	return providers.WorkItem{Labels: f.labels}, nil
}

// TestEscalatedFailureCommentNamesParkingLabel covers #5430 end to end through
// the terminal circuit breaker: a merge-review escalation parked
// goobers:merge-escalated is told to remove that label, and once the breaker
// trips the needs-human it applies is named too.
func TestEscalatedFailureCommentNamesParkingLabel(t *testing.T) {
	poster := &labeledPoster{labels: []string{providers.LabelMergeEscalated}}
	state := newFakeState()
	state.items["run-1"] = []Item{{ItemID: "pr/42", Repo: webRepo}}
	p := &Policy{Poster: poster, RunsDir: t.TempDir(), State: state}
	notify := p.TerminalNotifier(nil)
	lastComment := func() string {
		for i := len(poster.updates) - 1; i >= 0; i-- {
			if poster.updates[i].Comment != "" {
				return poster.updates[i].Comment
			}
		}
		t.Fatal("no failure-streak comment posted")
		return ""
	}

	if err := notify("run-1", journal.PhaseEscalated, "park-review"); err != nil {
		t.Fatalf("escalated: %v", err)
	}
	body := lastComment()
	if !strings.Contains(body, "Remove `goobers:merge-escalated` and re-approve to retry.") || strings.Contains(body, providers.LabelNeedsHuman) {
		t.Fatalf("below-threshold comment = %q, want merge-escalated named and no needs-human", body)
	}

	state.streaks["web#pr/42"] = FailureStreakThreshold - 1
	if err := notify("run-1", journal.PhaseEscalated, "park-review"); err != nil {
		t.Fatalf("escalated at threshold: %v", err)
	}
	if body := lastComment(); !strings.Contains(body, "Remove `goobers:needs-human` and `goobers:merge-escalated` and re-approve to retry.") {
		t.Fatalf("threshold comment = %q, want both park labels named", body)
	}
}

func TestTerminalNotifierJoinsErrors(t *testing.T) {
	p, _, state := newTestPolicy(t)
	state.itemsErr = errors.New("item repository unknown")
	notify := p.TerminalNotifier(func(string, journal.RunPhase, string) error { return errors.New("inner failed") })
	for _, phase := range []journal.RunPhase{journal.PhaseAborted, journal.PhaseCompleted} {
		err := notify("run-1", phase, "x")
		if err == nil || !strings.Contains(err.Error(), "item repository unknown") || !strings.Contains(err.Error(), "inner failed") {
			t.Fatalf("%s: err = %v, want breaker and inner errors joined", phase, err)
		}
	}
	if err := p.TerminalNotifier(nil)("run-1", journal.PhaseFailed, "x"); err != nil {
		t.Fatalf("nil inner on failed phase: %v", err)
	}
}

func TestResetCircuitBreakerReportsFailures(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	state.items["run-1"] = []Item{{ItemID: "42", Repo: webRepo}}
	state.writeErr = errors.New("store down")
	poster.listErr = errors.New("rate limited")
	err := p.resetCircuitBreaker(context.Background(), "run-1", "")
	if err == nil || !strings.Contains(err.Error(), "reset failure streak state on web#42") || !strings.Contains(err.Error(), "reset failure streak on web#42") {
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(state.cleared, []string{"web#42"}) {
		t.Fatalf("cleared = %v, want the pending park cleared regardless", state.cleared)
	}
}

func TestExistingFixRemovesReadyAndCriticalLabels(t *testing.T) {
	p, poster, _ := newTestPolicy(t)
	if err := p.ExistingFix(context.Background(), runner.ExistingFixOutcome{RepoRef: webRepoRef}); err != nil || len(poster.updates) != 0 {
		t.Fatalf("no item id: err=%v updates=%+v, want a no-op", err, poster.updates)
	}
	if err := p.ExistingFix(context.Background(), runner.ExistingFixOutcome{ItemID: "463", RepoRef: webRepoRef}); err != nil {
		t.Fatalf("ExistingFix: %v", err)
	}
	if len(poster.updates) != 1 {
		t.Fatalf("updates = %+v", poster.updates)
	}
	got := poster.updates[0]
	if got.ID != "463" || got.Repository != webRepo || !slices.Equal(got.RemoveLabels, []string{providers.LabelReady, providers.LabelCritical}) {
		t.Fatalf("request = %+v", got)
	}
	poster.updateErr = errors.New("provider unavailable")
	if err := p.ExistingFix(context.Background(), runner.ExistingFixOutcome{ItemID: "463", RepoRef: webRepoRef}); err == nil {
		t.Fatal("want update error")
	}
}
