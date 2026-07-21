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
	counts map[string]int
	known  bool
}

func (f fakeOpenPRCounter) OpenPRCount(_, workflow string) (int, bool) {
	return f.counts[workflow], f.known
}

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
		c.SetOpenPRCounter(fakeOpenPRCounter{counts: map[string]int{"implementation": 1}, known: true})
		ok, reason := c.Admit("implementation", capped, now)
		if ok || reason != ReasonOpenPRCap {
			t.Fatalf("ok=%v reason=%q, want blocked with %q", ok, reason, ReasonOpenPRCap)
		}
	})
	t.Run("admits below cap", func(t *testing.T) {
		c := NewConditions()
		c.SetOpenPRCounter(fakeOpenPRCounter{counts: map[string]int{"implementation": 0}, known: true})
		if ok, _ := c.Admit("implementation", capped, now); !ok {
			t.Fatal("expected admit below the cap")
		}
	})
	t.Run("fails open when count unknown", func(t *testing.T) {
		c := NewConditions()
		c.SetOpenPRCounter(fakeOpenPRCounter{counts: map[string]int{"implementation": 99}, known: false})
		if ok, _ := c.Admit("implementation", capped, now); !ok {
			t.Fatal("expected admit when the count is unknown (fail-open)")
		}
	})
	t.Run("no cap when MaxOpenPRs is unset", func(t *testing.T) {
		c := NewConditions()
		c.SetOpenPRCounter(fakeOpenPRCounter{counts: map[string]int{"implementation": 99}, known: true})
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
	t.Run("does not count tutor PRs against implementation", func(t *testing.T) {
		c := NewConditions()
		c.SetOpenPRCounter(fakeOpenPRCounter{
			counts: map[string]int{"implementation": 0, "tutor": 1},
			known:  true,
		})
		if ok, reason := c.Admit("implementation", capped, now); !ok {
			t.Fatalf("implementation: ok=%v reason=%q, want admitted", ok, reason)
		}
		if ok, reason := c.Admit("tutor", capped, now); ok || reason != ReasonOpenPRCap {
			t.Fatalf("tutor: ok=%v reason=%q, want blocked with %q", ok, reason, ReasonOpenPRCap)
		}
	})
}

type fakeOpenPRLister struct {
	heads  []string
	labels map[string][]string // head -> labels, for the exclusion tests
	err    error
	calls  int
}

func (f *fakeOpenPRLister) ListOpenPullRequests(_ context.Context, _ providers.RepositoryRef) ([]providers.OpenPRSummary, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	prs := make([]providers.OpenPRSummary, 0, len(f.heads))
	for _, h := range f.heads {
		prs = append(prs, providers.OpenPRSummary{Head: h, Labels: f.labels[h]})
	}
	return prs, nil
}

// TestOpenPRRefresherCountsRunBranchHeads is #353/#891: the refresher counts
// only each workflow's own run-branch PRs and reports "unknown" until the first
// poll completes.
func TestOpenPRRefresherCountsRunBranchHeads(t *testing.T) {
	lister := &fakeOpenPRLister{heads: []string{
		"goobers/implementation/run-1",
		"goobers/implementation/run-2",
		"goobers/tutor/run-3",
		"goobers/implementation-helper/run-4",
		"feature/human-authored",
	}}
	r := NewOpenPRRefresher(lister, providers.RepositoryRef{Owner: "o", Name: "n"}, time.Hour, nil, nil)

	if _, known := r.OpenPRCount("", "implementation"); known {
		t.Fatal("expected unknown before the first poll (Admit fails open until then)")
	}

	r.pollOnce(context.Background())
	for _, tc := range []struct {
		workflow string
		want     int
	}{
		{workflow: "implementation", want: 2},
		{workflow: "tutor", want: 1},
	} {
		n, known := r.OpenPRCount("", tc.workflow)
		if !known || n != tc.want {
			t.Errorf("%s: count=%d known=%v, want %d/true", tc.workflow, n, known, tc.want)
		}
	}
}

// TestOpenPRRefresherCountsPerGaggleNamespace is #1115: a gaggle that retuned
// its BranchNamespace (#1109) opens PRs under its own prefix, and the cap must
// count those — not the default "goobers/" heads (which belong to a different
// gaggle). Before the fix the counter matched only "goobers/…", so the acme
// gaggle's PRs were invisible and its cap silently ineffective.
func TestOpenPRRefresherCountsPerGaggleNamespace(t *testing.T) {
	lister := &fakeOpenPRLister{heads: []string{
		"goobers/implementation/run-1", // default-namespace gaggle
		"acme/implementation/run-2",    // acme-namespace gaggle
		"acme/implementation/run-3",
	}}
	// The map value need not be pre-normalized; BranchNameIn adds the trailing "/".
	r := NewOpenPRRefresher(lister, providers.RepositoryRef{Owner: "o", Name: "n"}, time.Hour, nil,
		map[string]string{"acme": "acme"})
	r.pollOnce(context.Background())

	// A gaggle absent from the map falls back to the default namespace and counts
	// only "goobers/…" heads — unchanged from pre-#1115 behavior.
	if n, known := r.OpenPRCount("default", "implementation"); !known || n != 1 {
		t.Errorf("default namespace: count=%d known=%v, want 1/true", n, known)
	}
	// The acme gaggle counts only its own "acme/…" heads (2), not the default one.
	if n, known := r.OpenPRCount("acme", "implementation"); !known || n != 2 {
		t.Errorf("acme namespace: count=%d known=%v, want 2/true", n, known)
	}
}

// TestOpenPRRefresherExcludesHumanParkedPRs is #986: a PR carrying an excluded
// (human-parked) label is dropped from the count, so a pool full of
// merge-escalated PRs the daemon can't drain doesn't starve new work — while
// PRs with other labels (or none) still count for backpressure.
func TestOpenPRRefresherExcludesHumanParkedPRs(t *testing.T) {
	const escalated = "goobers:merge-escalated"
	lister := &fakeOpenPRLister{
		heads: []string{
			"goobers/implementation/run-1", // counts
			"goobers/implementation/run-2", // parked → excluded
			"goobers/implementation/run-3", // needs-remediation still counts (drainable)
		},
		labels: map[string][]string{
			"goobers/implementation/run-2": {escalated},
			"goobers/implementation/run-3": {"goobers:needs-remediation"},
		},
	}
	r := NewOpenPRRefresher(lister, providers.RepositoryRef{Owner: "o", Name: "n"}, time.Hour, []string{escalated}, nil)

	r.pollOnce(context.Background())
	if n, known := r.OpenPRCount("", "implementation"); !known || n != 2 {
		t.Fatalf("count=%d known=%v, want 2 (run-1 + run-3; run-2 excluded as human-parked)", n, known)
	}
}

// TestOpenPRRefresherFailsOpenOnError is #353's fail-open contract: a poll error
// leaves the count "unknown" so Admit doesn't block on a transient GitHub hiccup.
func TestOpenPRRefresherFailsOpenOnError(t *testing.T) {
	lister := &fakeOpenPRLister{err: errors.New("github unavailable")}
	r := NewOpenPRRefresher(lister, providers.RepositoryRef{Owner: "o", Name: "n"}, time.Hour, nil, nil)

	r.pollOnce(context.Background())
	if _, known := r.OpenPRCount("", "implementation"); known {
		t.Fatal("expected unknown (fail-open) after a poll error")
	}

	// A later successful poll recovers the count.
	lister.err = nil
	lister.heads = []string{"goobers/implementation/run-9"}
	r.pollOnce(context.Background())
	if n, known := r.OpenPRCount("", "implementation"); !known || n != 1 {
		t.Fatalf("after recovery: n=%d known=%v, want 1/true", n, known)
	}
}

// TestOpenPRRefresherRunPollsAndStops confirms Run does an eager first poll and
// returns on context cancellation (its lifecycle under the daemon WaitGroup).
func TestOpenPRRefresherRunPollsAndStops(t *testing.T) {
	lister := &fakeOpenPRLister{heads: []string{"goobers/implementation/run-1"}}
	r := NewOpenPRRefresher(lister, providers.RepositoryRef{Owner: "o", Name: "n"}, time.Hour, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// The eager first poll makes the count known without waiting a full interval.
	deadline := time.After(2 * time.Second)
	for {
		if _, known := r.OpenPRCount("", "implementation"); known {
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
