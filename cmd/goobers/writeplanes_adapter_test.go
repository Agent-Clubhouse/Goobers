package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

func newClaimServiceFixture(t *testing.T) (*daemonClaimService, instance.Layout) {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	service := newDaemonClaimService(layout, instanceLog, func(now time.Time) ([]localscheduler.ClaimEntry, error) {
		return recoverClaims(layout, instanceLog, now, nil, nil)
	})
	if err := service.initializeMergeLock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.close)
	return service, layout
}

func claimPlaneRequest(runID string) httpapi.ClaimRequest {
	return httpapi.ClaimRequest{
		Gaggle: "example", Provider: "github", ItemID: "42",
		RunID: runID, Workflow: "implementation",
	}
}

type stubTriggerer struct {
	mu          sync.Mutex
	mints       int
	err         error
	lastOptions localscheduler.ManualTriggerOptions
}

func (s *stubTriggerer) mint(options localscheduler.ManualTriggerOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastOptions = options
	if s.err != nil {
		return "", s.err
	}
	s.mints++
	return fmt.Sprintf("run-%d", s.mints), nil
}

func (s *stubTriggerer) TriggerWithDispatchContextOptions(_, _ context.Context, _ string, _ time.Time, options localscheduler.ManualTriggerOptions) (string, error) {
	return s.mint(options)
}
func (s *stubTriggerer) TriggerExactWithDispatchContextOptions(_, _ context.Context, _ localscheduler.WorkflowIdentity, _ time.Time, options localscheduler.ManualTriggerOptions) (string, error) {
	return s.mint(options)
}
func (s *stubTriggerer) TriggerPriorityWithDispatchContext(_, _ context.Context, _ localscheduler.WorkflowIdentity, _ string, _ time.Time) (string, error) {
	return s.mint(localscheduler.ManualTriggerOptions{})
}

type barrierTriggerer struct {
	entered chan struct{}
	release chan struct{}
	mints   atomic.Int32
}

func (b *barrierTriggerer) mint() (string, error) {
	n := b.mints.Add(1)
	if n == 1 {
		close(b.entered)
		<-b.release
	}
	return fmt.Sprintf("run-%d", n), nil
}
func (b *barrierTriggerer) TriggerWithDispatchContextOptions(_, _ context.Context, _ string, _ time.Time, _ localscheduler.ManualTriggerOptions) (string, error) {
	return b.mint()
}
func (b *barrierTriggerer) TriggerExactWithDispatchContextOptions(_, _ context.Context, _ localscheduler.WorkflowIdentity, _ time.Time, _ localscheduler.ManualTriggerOptions) (string, error) {
	return b.mint()
}
func (b *barrierTriggerer) TriggerPriorityWithDispatchContext(_, _ context.Context, _ localscheduler.WorkflowIdentity, _ string, _ time.Time) (string, error) {
	return b.mint()
}
