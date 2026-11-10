package terminalreport

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

type review7671LeasePoster struct {
	mu           sync.Mutex
	comments     []providers.Comment
	calls        int
	entered      chan struct{}
	release      chan struct{}
	firstContext context.Context
}

func (p *review7671LeasePoster) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providers.Comment(nil), p.comments...), nil
}
func (p *review7671LeasePoster) AuthenticatedIdentity(context.Context) (providers.ADOIdentity, error) {
	return providers.ADOIdentity{ID: "our-guid"}, nil
}
func (p *review7671LeasePoster) UpdateComment(context.Context, providers.RepositoryRef, string, string) error {
	return errors.New("editing unsupported")
}
func (p *review7671LeasePoster) UpdateWorkItem(ctx context.Context, r providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		p.firstContext = ctx
		close(p.entered)
		<-p.release
	}
	if err := ctx.Err(); err != nil {
		return providers.WorkItem{}, err
	}
	p.mu.Lock()
	p.comments = append(p.comments, providers.Comment{Body: r.Comment, AuthorID: "our-guid"})
	p.mu.Unlock()
	return providers.WorkItem{}, nil
}
func TestReview7671LeaseExpiryMustNotOverlapLivePublisher(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	q := Queue{Dir: t.TempDir(), Now: func() time.Time { return time.Unix(0, clock.Load()) }}
	e := Effect{Kind: WorkFailure, Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Name: "app"}, ItemID: "42", RunID: "run-2", Count: 2}
	p := &review7671LeasePoster{entered: make(chan struct{}), release: make(chan struct{})}
	apply := func(ctx context.Context, e Effect) error { return e.Apply(ctx, p) }
	done := make(chan error, 1)
	go func() { done <- q.Publish(context.Background(), e, apply) }()
	<-p.entered
	// A forward wall-clock adjustment expires the JSON lease but not the
	// monotonic 30-second context deadline of the in-flight provider request.
	clock.Add(int64(2 * time.Minute))
	restarted := Queue{Dir: q.Dir, Now: q.Now}
	replayErr := restarted.Reconcile(context.Background(), 1, apply)
	activeErr := p.firstContext.Err()
	close(p.release)
	initialErr := <-done
	if replayErr != nil || initialErr != nil {
		t.Fatalf("publish=%v replay=%v", initialErr, replayErr)
	}
	p.mu.Lock()
	calls, comments := p.calls, len(p.comments)
	p.mu.Unlock()
	if calls != 1 || comments != 1 {
		t.Fatalf("lease expiry admitted overlapping publications: calls=%d comments=%d original context before release=%v", calls, comments, activeErr)
	}
}

func TestReportLeaseCapturesMonotonicCallBudget(t *testing.T) {
	wall := time.Unix(1800000000, 0)
	q := Queue{Dir: t.TempDir(), Now: func() time.Time { return wall }}
	e := Effect{Kind: WorkFailure, ItemID: "lease-budget"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Publish(ctx, e, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	before := time.Now()
	claimed, err := q.claim(context.Background(), effectKey(e))
	if err != nil {
		t.Fatal(err)
	}
	if claimed.LeaseUntil.Sub(wall) != reportLeaseDuration || reportLeaseDuration < reportCallTimeout+30*time.Second {
		t.Fatal("lease lacks skew margin")
	}
	if claimed.callDeadline.After(time.Now().Add(reportCallTimeout)) || claimed.callDeadline.Before(before.Add(reportCallTimeout)) {
		t.Fatal("call deadline not captured at acquisition")
	}
	wall = wall.Add(24 * time.Hour)
	callCtx, stop := context.WithDeadline(context.Background(), claimed.callDeadline)
	defer stop()
	if callCtx.Err() != nil {
		t.Fatal("wall jump changed monotonic deadline")
	}
	expired := claimed
	expired.callDeadline = before.Add(-time.Second)
	expiredCtx, stopExpired := context.WithDeadline(context.Background(), expired.callDeadline)
	defer stopExpired()
	if !errors.Is(expiredCtx.Err(), context.DeadlineExceeded) {
		t.Fatal("consumed acquisition budget was extended")
	}
}
