//go:build cloudintegration

package terminalstate_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/blockedrecordspg"
	"github.com/goobers/goobers/internal/cloud/failurestreakpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/terminalstate"
	"github.com/goobers/goobers/internal/escalationnotify"
	"github.com/goobers/goobers/internal/terminalhooks"
	"github.com/goobers/goobers/providers"
)

var code = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

type poster struct {
	mu      sync.Mutex
	updates []providers.UpdateWorkItemRequest
}

func (p *poster) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return nil, nil
}

func (p *poster) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updates = append(p.updates, req)
	return providers.WorkItem{}, nil
}

func (p *poster) UpdateComment(context.Context, providers.RepositoryRef, string, string) error {
	return nil
}

func (p *poster) parked() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, u := range p.updates {
		if slices.Contains(u.AddLabels, providers.LabelNeedsHuman) && slices.Contains(u.RemoveLabels, providers.LabelReady) {
			return true
		}
	}
	return false
}

func policyFor(manager *lease.Manager, handle *lease.Handle, pool failurestreakpg.Store, p *poster) *escalationnotify.Policy {
	return &escalationnotify.Policy{
		Poster: p, Gaggle: "web",
		State: terminalstate.State{
			State: blockedrecordspg.State{
				Store: blockedrecordspg.Store{Fence: manager}, Handle: handle,
				Claimed: func(string) ([]string, error) { return []string{"42"}, nil },
			},
			Streaks:  pool,
			CodeRepo: code,
		},
	}
}

func failed(run string) terminalhooks.FailedOutcome {
	return terminalhooks.FailedOutcome{RunID: run, RepoRef: apiv1.RepoRef{Provider: "github", Owner: "acme", Name: "web"}, Stage: "implement", Code: "run_failed"}
}

// The breaker counts consecutive failures across lease epochs and parks at the
// threshold, with every store call running as goobers_controller.
func TestFailureStreakCountsAcrossEpochsAndParksAtTheThreshold(t *testing.T) {
	pool := pgtest.Pool(t, "tsst")
	controller := pgtest.ControllerPool(t, pool)
	oldManager := pgtest.ControllerManager(t, pool, "old", 0)
	old := pgtest.Acquire(t, oldManager)
	streaks := failurestreakpg.Store{Pool: controller, Fence: oldManager}
	p := &poster{}
	ctx := providers.WithAttributionContext(t.Context(), providers.Attribution{Schema: 1, Goobers: true, Gaggle: "web", Goober: "runner"})
	for _, run := range []string{"r1", "r2"} {
		if err := policyFor(oldManager, old, streaks, p).Failed(ctx, failed(run)); err != nil {
			t.Fatal(err)
		}
	}
	if p.parked() {
		t.Fatal("parked before the threshold")
	}
	newManager := pgtest.ControllerManager(t, pool, "new", 0)
	successor := pgtest.Takeover(t, pool, newManager)
	streaks = failurestreakpg.Store{Pool: controller, Fence: newManager}
	if err := policyFor(newManager, successor, streaks, p).Failed(ctx, failed("r3")); err != nil {
		t.Fatal(err)
	}
	if !p.parked() {
		t.Fatalf("the third consecutive failure did not park the item: %+v", p.updates)
	}
	if n, err := streaks.Load(t.Context(), pgtest.InstanceID, failurestreakpg.Work, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}, "42"); err != nil || n != 3 {
		t.Fatalf("streak = %d, %v; want 3", n, err)
	}
	// A stale leader's failure is refused and counts nothing.
	if err := policyFor(oldManager, old, failurestreakpg.Store{Pool: controller, Fence: oldManager}, p).Failed(ctx, failed("r4")); err == nil {
		t.Fatal("a stale leader charged the streak")
	}
	// A zero write (completed run) resets: the row is gone.
	if err := streaks.Write(t.Context(), successor, failurestreakpg.Work, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}, "42", 0, "r5", ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := streaks.Load(t.Context(), pgtest.InstanceID, failurestreakpg.Work, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}, "42"); n != 0 {
		t.Fatalf("streak after reset = %d", n)
	}
}
