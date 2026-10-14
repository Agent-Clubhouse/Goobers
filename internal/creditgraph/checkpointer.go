package creditgraph

import (
	"context"
	"sync"
)

// Bounds on the checkpointer's in-memory state. A dropped observation is
// harmless: the run's next journal append re-enqueues it, and the end-of-run
// record composes whatever checkpoints exist.
const (
	maxPendingStageCheckpoints = 1024
	maxSettledStageRuns        = 4096
)

// StageCheckpointer writes stage-granular attribution checkpoints off the
// journal append path. Observe only records that a run advanced; a single
// background worker, started on demand and exiting when idle, does the
// reading and writing. Failures are reported and never reach the run.
type StageCheckpointer struct {
	resolve func(runID string) (string, error)
	report  func(runID string, err error)

	mu      sync.Mutex
	queue   []string
	pending map[string]struct{}
	settled map[string]struct{}
	running bool
	worker  sync.WaitGroup
}

// NewStageCheckpointer returns a checkpointer that resolves a run ID to its
// run directory and reports (never returns) checkpoint failures.
func NewStageCheckpointer(resolve func(runID string) (string, error), report func(runID string, err error)) *StageCheckpointer {
	return &StageCheckpointer{
		resolve: resolve, report: report,
		pending: map[string]struct{}{}, settled: map[string]struct{}{},
	}
}

// Observe is a journal append observer. It never blocks on analysis.
func (c *StageCheckpointer) Observe(_ context.Context, runID string, _ uint64) {
	if c == nil || runID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.settled[runID]; ok {
		return
	}
	if _, ok := c.pending[runID]; ok || len(c.queue) >= maxPendingStageCheckpoints {
		return
	}
	c.pending[runID] = struct{}{}
	c.queue = append(c.queue, runID)
	if !c.running {
		c.running = true
		c.worker.Add(1)
		go c.drain()
	}
}

func (c *StageCheckpointer) drain() {
	defer c.worker.Done()
	for {
		c.mu.Lock()
		if len(c.queue) == 0 {
			c.running = false
			c.mu.Unlock()
			return
		}
		runID := c.queue[0]
		c.queue = c.queue[1:]
		delete(c.pending, runID)
		c.mu.Unlock()
		c.checkpoint(runID)
	}
}

func (c *StageCheckpointer) checkpoint(runID string) {
	runDir, err := c.resolve(runID)
	if err != nil {
		c.fail(runID, err)
		c.settle(runID)
		return
	}
	result, err := WriteStageCheckpoints(runDir)
	if err != nil {
		c.fail(runID, err)
	}
	if result.Settled {
		c.settle(runID)
	}
}

func (c *StageCheckpointer) fail(runID string, err error) {
	if c.report != nil {
		c.report(runID, err)
	}
}

func (c *StageCheckpointer) settle(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.settled) >= maxSettledStageRuns {
		c.settled = map[string]struct{}{}
	}
	c.settled[runID] = struct{}{}
}
