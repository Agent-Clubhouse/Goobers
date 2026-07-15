package localscheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

type fakeOpenPRCounter struct {
	n     int
	known bool
}

func (f fakeOpenPRCounter) OpenPRCount() (int, bool) { return f.n, f.known }

// TestAdmitOpenPRCap is #353: the MaxOpenPRs cap blocks a new run once the
// counter reports the workflow's open PRs at the cap, admits below it, and
// fails OPEN (admits) whenever the count is unknown, the cap is unset, or no
// counter is wired — a GitHub hiccup must never stall a tick.
func TestAdmitOpenPRCap(t *testing.T) {
	now := time.Now()
	// MaxConcurrentRuns high so it never masks the open-PR check under test.
	capped := apiv1.ReadinessConditions{MaxConcurrentRuns: 10, MaxOpenPRs: 1}

	t.Run("blocks at cap", func(t *testing.T) {
		c := NewConditions()
		c.SetOpenPRCounter(fakeOpenPRCounter{n: 1, known: true})
		ok, reason := c.Admit("implementation", capped, now)
		if ok || reason != ReasonOpenPRCap {
			t.Fatalf("ok=%v reason=%q, want blocked with %q", ok, reason, ReasonOpenPRCap)
		}
	})
	t.Run("admits below cap", func(t *testing.T) {
		c := NewConditions()
		c.SetOpenPRCounter(fakeOpenPRCounter{n: 0, known: true})
		if ok, _ := c.Admit("implementation", capped, now); !ok {
			t.Fatal("expected admit below the cap")
		}
	})
	t.Run("fails open when count unknown", func(t *testing.T) {
		c := NewConditions()
		c.SetOpenPRCounter(fakeOpenPRCounter{n: 99, known: false})
		if ok, _ := c.Admit("implementation", capped, now); !ok {
			t.Fatal("expected admit when the count is unknown (fail-open)")
		}
	})
	t.Run("no cap when MaxOpenPRs is unset", func(t *testing.T) {
		c := NewConditions()
		c.SetOpenPRCounter(fakeOpenPRCounter{n: 99, known: true})
		if ok, _ := c.Admit("implementation", apiv1.ReadinessConditions{MaxConcurrentRuns: 10}, now); !ok {
			t.Fatal("expected admit when MaxOpenPRs is unset (opt-in)")
		}
	})
	t.Run("no counter wired leaves the cap unenforced", func(t *testing.T) {
		c := NewConditions()
		if ok, _ := c.Admit("implementation", capped, now); !ok {
			t.Fatal("expected admit when no counter is wired (fail-open)")
		}
	})
}

type fakeOpenPRLister struct {
	heads []string
	err   error
	calls int
}

func (f *fakeOpenPRLister) ListOpenPullRequestHeads(_ context.Context, _ providers.RepositoryRef) ([]string, error) {
	f.calls++
	return f.heads, f.err
}

// TestOpenPRRefresherCountsRunBranchHeads is #353: the refresher counts only the
// open PRs under the goobers/ run-branch namespace (providers.BranchName), and
// reports "unknown" until the first poll completes.
func TestOpenPRRefresherCountsRunBranchHeads(t *testing.T) {
	lister := &fakeOpenPRLister{heads: []string{
		"goobers/implementation/run-1",
		"goobers/implementation/run-2",
		"goobers/backlog-curation/run-3", // still a run-branch PR (goobers/ prefix)
		"feature/human-authored",         // not a loop PR — excluded
		"dependabot/go_modules/x",        // not a loop PR — excluded
	}}
	r := NewOpenPRRefresher(lister, providers.RepositoryRef{Owner: "o", Name: "n"}, time.Hour)

	if _, known := r.OpenPRCount(); known {
		t.Fatal("expected unknown before the first poll (Admit fails open until then)")
	}

	r.pollOnce(context.Background())
	n, known := r.OpenPRCount()
	if !known {
		t.Fatal("expected known after a successful poll")
	}
	if n != 3 {
		t.Fatalf("count = %d, want 3 (only goobers/ run-branch heads)", n)
	}
}

// TestOpenPRRefresherFailsOpenOnError is #353's fail-open contract: a poll error
// leaves the count "unknown" so Admit doesn't block on a transient GitHub hiccup.
func TestOpenPRRefresherFailsOpenOnError(t *testing.T) {
	lister := &fakeOpenPRLister{err: errors.New("github unavailable")}
	r := NewOpenPRRefresher(lister, providers.RepositoryRef{Owner: "o", Name: "n"}, time.Hour)

	r.pollOnce(context.Background())
	if _, known := r.OpenPRCount(); known {
		t.Fatal("expected unknown (fail-open) after a poll error")
	}

	// A later successful poll recovers the count.
	lister.err = nil
	lister.heads = []string{"goobers/implementation/run-9"}
	r.pollOnce(context.Background())
	if n, known := r.OpenPRCount(); !known || n != 1 {
		t.Fatalf("after recovery: n=%d known=%v, want 1/true", n, known)
	}
}

// TestOpenPRRefresherRunPollsAndStops confirms Run does an eager first poll and
// returns on context cancellation (its lifecycle under the daemon WaitGroup).
func TestOpenPRRefresherRunPollsAndStops(t *testing.T) {
	lister := &fakeOpenPRLister{heads: []string{"goobers/implementation/run-1"}}
	r := NewOpenPRRefresher(lister, providers.RepositoryRef{Owner: "o", Name: "n"}, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// The eager first poll makes the count known without waiting a full interval.
	deadline := time.After(2 * time.Second)
	for {
		if _, known := r.OpenPRCount(); known {
			break
		}
		select {
		case <-deadline:
			t.Fatal("refresher never completed its eager first poll")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
