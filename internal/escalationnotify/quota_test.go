package escalationnotify

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/providers"
)

func TestFailureProjectionReplaysWithoutClaimsOrRechargingStreak(t *testing.T) {
	for _, provider := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		t.Run(string(provider), func(t *testing.T) {
			p, poster, state := newTestPolicy(t)
			now := time.Now()
			queue := &terminalreport.Queue{Dir: t.TempDir(), Now: func() time.Time { return now }}
			p.Reports = queue
			repo := providers.RepositoryRef{Provider: provider, Owner: "acme", Project: "project", Name: "app"}
			state.items["run-1"] = []Item{{Repo: repo, ItemID: "42"}}
			poster.listErr = &providers.RateLimitError{Provider: provider, Reset: now.Add(time.Minute)}
			if err := p.applyCircuitBreaker(context.Background(), "run-1", "ci-poll", ""); err == nil {
				t.Fatal("expected deferred comment")
			}
			if state.streaks["app#42"] != 1 {
				t.Fatal("streak not charged")
			}
			delete(state.items, "run-1") // Finalization released claims; replay still owes the comment.
			poster.listErr = nil
			now = now.Add(2 * time.Minute)
			restart := terminalreport.Queue{Dir: queue.Dir, Now: queue.Now}
			if err := restart.Reconcile(context.Background(), 8, func(ctx context.Context, e terminalreport.Effect) error { return e.Apply(ctx, poster) }); err != nil {
				t.Fatal(err)
			}
			if state.streaks["app#42"] != 1 {
				t.Fatal("replay recharged streak")
			}
			if len(poster.updates) != 1 || poster.updates[0].Repository != repo || !strings.Contains(poster.updates[0].Comment, `data-count="1"`) {
				t.Fatalf("projection=%+v", poster.updates)
			}
		})
	}
}

// guardedState is a State whose controller re-fires terminals: it remembers
// which runs already charged which streak.
type guardedState struct {
	*fakeState
	charged map[string]bool
}

func (g guardedState) StreakChargedBy(_ context.Context, infra bool, _ providers.RepositoryRef, itemID, runID string) (bool, error) {
	return g.charged[fmt.Sprintf("%v/%s/%s", infra, itemID, runID)], nil
}

func TestAStreakChargeGuardSkipsARunThatAlreadyCharged(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	state.items["run-1"] = []Item{{Repo: repo, ItemID: "42"}}
	p.State = guardedState{fakeState: state, charged: map[string]bool{"false/42/run-1": true}}
	if err := p.applyCircuitBreaker(context.Background(), "run-1", "ci", ""); err != nil {
		t.Fatal(err)
	}
	if state.streaks["app#42"] != 0 || len(poster.updates) != 0 {
		t.Fatalf("a re-fired charge was applied again: streak %v, %d writes", state.streaks, len(poster.updates))
	}
	// A different run, and the other streak kind, still charge.
	state.items["run-2"] = []Item{{Repo: repo, ItemID: "42"}}
	if err := p.applyCircuitBreaker(context.Background(), "run-2", "ci", ""); err != nil {
		t.Fatal(err)
	}
	if state.streaks["app#42"] != 1 {
		t.Fatalf("a new run did not charge: %v", state.streaks)
	}
}
