package escalationnotify

import (
	"context"
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
			p.Reports = &terminalreport.Queue{Dir: t.TempDir(), Now: func() time.Time { return now }}
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
			restart := terminalreport.Queue{Dir: p.Reports.Dir, Now: p.Reports.Now}
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
