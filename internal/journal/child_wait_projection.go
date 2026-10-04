package journal

import (
	"fmt"
	"slices"
)

// PendingChildStage is a host wait bound to its exact durable stage start.
type PendingChildStage struct {
	Header  ChildWaitHeader
	Marker  Event
	Started Event
}

// ChildWaitProjection distinguishes branch custody from whole-run suspension.
// RunnableBranches includes declared siblings that have not yet started.
type ChildWaitProjection struct {
	Waits            map[int]PendingChildStage
	RunnableBranches []int
	Parallel         string
}

// Parked is true only when there is pending child custody and no runnable owner.
func (p ChildWaitProjection) Parked() bool {
	return len(p.Waits) > 0 && len(p.RunnableBranches) == 0
}

type childWaitProjector struct {
	waits    map[int]PendingChildStage
	started  map[int]Event
	owners   map[int]bool
	parallel string
}

func newChildWaitProjector() *childWaitProjector {
	return &childWaitProjector{waits: map[int]PendingChildStage{}, started: map[int]Event{}, owners: map[int]bool{0: true}}
}

// ProjectChildWaits validates host wait/continuation pairs independently by
// branch. It supports the runner's non-nested parallel graph and fails closed
// if a parallel declaration cannot account for every owner.
func ProjectChildWaits(events []Event) (ChildWaitProjection, error) {
	return projectChildWaits(events, nil)
}

func projectChildWaits(events []Event, observe func(Event, *childWaitProjector) error) (ChildWaitProjection, error) {
	p := newChildWaitProjector()
	if !hasChildWait(events) {
		return p.projection(), nil
	}
	for _, event := range events {
		if err := p.consume(event); err != nil {
			return ChildWaitProjection{}, err
		}
		if observe != nil {
			if err := observe(event, p); err != nil {
				return ChildWaitProjection{}, err
			}
		}
	}
	return p.projection(), nil
}

func hasChildWait(events []Event) bool {
	for _, event := range events {
		if event.Type == EventRunnerAnnotation && (event.Runner["kind"] == ChildWaitKind || event.Runner["kind"] == ChildContinuedKind) {
			return true
		}
	}
	return false
}

func (p *childWaitProjector) projection() ChildWaitProjection {
	result := ChildWaitProjection{Waits: p.waits, Parallel: p.parallel}
	for branch := range p.owners {
		if _, waiting := p.waits[branch]; !waiting {
			result.RunnableBranches = append(result.RunnableBranches, branch)
		}
	}
	slices.Sort(result.RunnableBranches)
	return result
}

func (p *childWaitProjector) consume(event Event) error {
	if p.parallel != "" && (event.Type == EventBranchStarted || event.Type == EventStageStarted) && !p.owners[event.Branch] {
		return fmt.Errorf("runner: child wait stage is outside unfinished declared branches")
	}
	switch event.Type {
	case EventParallelStarted:
		return p.beginParallel(event)
	case EventParallelFinished:
		if p.parallel != "" && event.Parallel != p.parallel || len(p.waits) != 0 {
			return fmt.Errorf("runner: parallel finished with unresolved child custody")
		}
		p.parallel, p.owners, p.started = "", map[int]bool{0: true}, map[int]Event{}
	case EventBranchStarted:
		// A filtered branch history has no root parallel declaration.
		if p.parallel == "" {
			delete(p.owners, 0)
			p.owners[event.Branch] = true
		}
	case EventBranchFinished:
		if _, waiting := p.waits[event.Branch]; waiting {
			return fmt.Errorf("runner: branch finished with unresolved child custody")
		}
		delete(p.owners, event.Branch)
		delete(p.started, event.Branch)
	case EventStageStarted:
		if _, waiting := p.waits[event.Branch]; waiting {
			return fmt.Errorf("runner: child wait has an unacknowledged replacement attempt")
		}
		p.owners[event.Branch], p.started[event.Branch] = true, event
	case EventStageFinished:
		if wait, ok := p.waits[event.Branch]; ok && event.Stage == wait.Marker.Stage && (event.Attempt == 0 || event.Attempt == wait.Marker.Attempt) {
			delete(p.waits, event.Branch)
		}
	case EventRunFinished:
		// A later authorized human continuation can restore a recovered wait
		// against the original immutable start without minting a new attempt.
		// Retain declared sibling ownership too: restoring one branch must
		// not release the run permit before the others are accounted for.
		p.waits = map[int]PendingChildStage{}
	case EventRunnerAnnotation:
		return p.annotation(event)
	}
	return nil
}

func (p *childWaitProjector) beginParallel(event Event) error {
	if p.parallel != "" || len(p.waits) != 0 || event.Parallel == "" || len(event.Completeness) == 0 || len(event.Completeness) > 128 {
		return fmt.Errorf("runner: child wait parallel ownership is unavailable")
	}
	owners := map[int]bool{}
	for _, branch := range event.Completeness {
		if branch.Branch <= 0 || owners[branch.Branch] {
			return fmt.Errorf("runner: child wait parallel has invalid owners")
		}
		owners[branch.Branch] = true
	}
	p.parallel, p.owners, p.started = event.Parallel, owners, map[int]Event{}
	return nil
}

func (p *childWaitProjector) annotation(event Event) error {
	kind, _ := event.Runner["kind"].(string)
	wait, waiting := p.waits[event.Branch]
	if kind == ChildContinuedKind {
		if !waiting || event.Runner["requestId"] != wait.Header.Request.RequestID || event.Stage != wait.Marker.Stage || event.Attempt != wait.Marker.Attempt {
			return fmt.Errorf("runner: child continuation does not match its branch wait")
		}
		delete(p.waits, event.Branch)
	}
	if kind != ChildWaitKind {
		return nil
	}
	started := p.started[event.Branch]
	header, err := DecodeChildWaitHeader(event, started)
	if err != nil {
		return err
	}
	if waiting && wait.Header.Request != header.Request {
		return fmt.Errorf("runner: overlapping child waits on one branch")
	}
	p.waits[event.Branch] = PendingChildStage{Header: *header, Marker: event, Started: started}
	return nil
}
