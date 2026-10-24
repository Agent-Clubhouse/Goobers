package parking

import (
	"context"
	"errors"
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/terminalhooks"
	"github.com/goobers/goobers/providers"
)

type poster struct {
	reqs []providers.UpdateWorkItemRequest
	err  error
}

func (p *poster) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	p.reqs = append(p.reqs, req)
	return providers.WorkItem{}, p.err
}

type state struct {
	claimed []string
	cycle   blockedcycle.Result
	blocks  []Block
	err     error
}

func (s *state) ClaimedItemIDs(string) ([]string, error) { return s.claimed, nil }
func (s *state) BacklogRepository(r providers.RepositoryRef) providers.RepositoryRef {
	r.Project = "backlog"
	return r
}
func (s *state) RecordBlock(b Block) (blockedcycle.Result, error) {
	s.blocks = append(s.blocks, b)
	return s.cycle, s.err
}

var code = apiv1.RepoRef{Provider: "github", Owner: "acme", Name: "web"}

func TestABlockWithANamedBlockerRecordsItAndParksOnSibling(t *testing.T) {
	p, s := &poster{}, &state{}
	o := terminalhooks.BlockedOutcome{RunID: "r", RepoRef: code, Stage: "implement", ItemID: "5", Blockers: []string{"5", "7"}, Reason: "waits"}
	if err := Blocked(t.Context(), p, s, o); err != nil {
		t.Fatal(err)
	}
	if len(s.blocks) != 1 || !slices.Equal(s.blocks[0].Blockers, []string{"7"}) || s.blocks[0].Repository.Project != "backlog" {
		t.Fatalf("recorded %+v, want the self-blocker dropped and the backlog repository", s.blocks)
	}
	if len(p.reqs) != 1 || !slices.Contains(p.reqs[0].AddLabels, providers.LabelBlockedOnSibling) ||
		!slices.Contains(p.reqs[0].RemoveLabels, providers.LabelReady) || !slices.Contains(p.reqs[0].RemoveLabels, providers.LabelClaimed) {
		t.Fatalf("park request = %+v", p.reqs)
	}
}

func TestAnUnattributedBlockParksNeedsHumanAndRecordsNothing(t *testing.T) {
	p, s := &poster{}, &state{}
	if err := Blocked(t.Context(), p, s, terminalhooks.BlockedOutcome{RunID: "r", RepoRef: code, ItemID: "5", Blockers: []string{"5"}}); err != nil {
		t.Fatal(err)
	}
	if len(s.blocks) != 0 || len(p.reqs) != 1 || !slices.Contains(p.reqs[0].AddLabels, providers.LabelNeedsHuman) {
		t.Fatalf("blocks %v requests %+v", s.blocks, p.reqs)
	}
}

func TestACycleEscalatesEveryMemberInsteadOfParkingOnSibling(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	cycle := blockedcycle.Result{
		Affected: []blockedcycle.Node{{Repository: repo, ItemID: "1"}, {Repository: repo, ItemID: "2"}},
		Paths:    [][]string{{"1", "2", "1"}},
	}
	p, s := &poster{}, &state{cycle: cycle}
	if err := Blocked(t.Context(), p, s, terminalhooks.BlockedOutcome{RunID: "r", RepoRef: code, ItemID: "1", Blockers: []string{"2"}}); err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) < 2 {
		t.Fatalf("requests = %d, want at least one per member", len(p.reqs))
	}
	for _, req := range p.reqs {
		if req.Comment == "" || !slices.Contains(req.AddLabels, providers.LabelNeedsHuman) || slices.Contains(req.AddLabels, providers.LabelBlockedOnSibling) {
			t.Fatalf("cycle escalation = %+v", req)
		}
	}
}

// observingState is a state that also hears when a cycle was escalated.
type observingState struct {
	state
	escalated []blockedcycle.Result
}

func (s *observingState) CycleEscalated(c blockedcycle.Result) error {
	s.escalated = append(s.escalated, c)
	return nil
}

// A state keeping per-cycle markers retires them only after the escalation's
// writes all succeeded: a failed escalation must leave them standing.
func TestTheEscalationObserverHearsOnlyOfSuccessfulEscalations(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	cycle := blockedcycle.Result{Affected: []blockedcycle.Node{{Repository: repo, ItemID: "1"}, {Repository: repo, ItemID: "2"}}, Paths: [][]string{{"1", "2", "1"}}}
	outcome := terminalhooks.BlockedOutcome{RunID: "r", RepoRef: code, ItemID: "1", Blockers: []string{"2"}}

	failing, s := &poster{err: errors.New("provider down")}, &observingState{state: state{cycle: cycle}}
	if err := Blocked(t.Context(), failing, s, outcome); err == nil {
		t.Fatal("a failed escalation was not reported")
	}
	if len(s.escalated) != 0 {
		t.Fatalf("observer heard of %d escalations after a failed one, want none", len(s.escalated))
	}

	s = &observingState{state: state{cycle: cycle}}
	if err := Blocked(t.Context(), &poster{}, s, outcome); err != nil {
		t.Fatal(err)
	}
	if len(s.escalated) != 1 || len(s.escalated[0].Affected) != 2 {
		t.Fatalf("observer heard %+v, want the escalated cycle once", s.escalated)
	}
}

func TestARunWithNoDrivingItemDoesNothingAndClaimsResolveItems(t *testing.T) {
	p, s := &poster{}, &state{}
	if err := Blocked(t.Context(), p, s, terminalhooks.BlockedOutcome{RunID: "r", RepoRef: code}); err != nil || len(p.reqs) != 0 {
		t.Fatalf("producer run: %v, %v", err, p.reqs)
	}
	s.claimed = []string{"8", "9"}
	if err := Blocked(t.Context(), p, s, terminalhooks.BlockedOutcome{RunID: "r", RepoRef: code}); err != nil || len(p.reqs) != 2 {
		t.Fatalf("claimed items: %v, %d requests", err, len(p.reqs))
	}
}

func TestProviderFailuresAreJoinedNotFatalPerItem(t *testing.T) {
	boom := errors.New("provider down")
	p, s := &poster{err: boom}, &state{claimed: []string{"8", "9"}}
	err := Blocked(t.Context(), p, s, terminalhooks.BlockedOutcome{RunID: "r", RepoRef: code})
	if !errors.Is(err, boom) || len(p.reqs) != 2 {
		t.Fatalf("err %v after %d requests, want both items attempted and the failure joined", err, len(p.reqs))
	}
	if err := Blocked(t.Context(), &poster{}, &state{}, terminalhooks.BlockedOutcome{RunID: "r", ItemID: "1"}); err == nil {
		t.Fatal("a blocked outcome with no repository was accepted")
	}
}

func TestExistingFixStripsTheReclaimLabels(t *testing.T) {
	p := &poster{}
	if err := ExistingFix(t.Context(), p, terminalhooks.ExistingFixOutcome{RepoRef: code}); err != nil || len(p.reqs) != 0 {
		t.Fatalf("no item: %v %v", err, p.reqs)
	}
	if err := ExistingFix(t.Context(), p, terminalhooks.ExistingFixOutcome{RepoRef: code, ItemID: "3"}); err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 1 || !slices.Equal(p.reqs[0].RemoveLabels, []string{providers.LabelReady, providers.LabelCritical}) {
		t.Fatalf("requests = %+v", p.reqs)
	}
}
