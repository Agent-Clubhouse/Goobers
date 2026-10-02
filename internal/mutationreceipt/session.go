package mutationreceipt

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrUnresolved means current provider evidence cannot establish a prior outcome.
// Callers must stop rather than blindly retry an uncertain mutation.
var ErrUnresolved = errors.New("provider mutation requires reconciliation")

// Session serializes durable mutation capture and provider reconciliation. It
// does not enable provider hooks; their production wiring remains disabled.
type Session struct {
	mu       sync.Mutex
	runID    string
	recorder Recorder
	history  []Receipt
}

// NewSession copies the immutable continuation lineage into a run-local session.
func NewSession(runID string, recorder Recorder, history []Receipt) *Session {
	return &Session{runID: runID, recorder: recorder, history: append([]Receipt(nil), history...)}
}

// Execute serializes effects within one provider session. Every semantic match
// must be checked against fresh provider state, even when locally completed.
// It does not retry uncertain writes. Successful reconciliation upgrades only
// the observed invocation, preserving its original run and opaque identity.
func (s *Session) Execute(ctx context.Context, identity Identity, reconcile func(context.Context, []Receipt) (*Receipt, error), action func(context.Context, Receipt) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recorder == nil || s.runID == "" || reconcile == nil || action == nil {
		return fmt.Errorf("mutation reconciliation requires durable recorder and run identity")
	}
	matches, err := matchingReceipts(s.history, identity)
	if err != nil {
		return err
	}
	if len(matches) > 0 {
		observed, err := reconcile(FreshRead(ctx), matches)
		if err != nil {
			return err
		}
		if observed == nil || !containsReceipt(matches, *observed) {
			return fmt.Errorf("%w: %s", ErrUnresolved, identity.Action)
		}
		if observed.Phase == "completed" {
			return nil
		}
		completed := *observed
		completed.Phase = "completed"
		return s.record(context.WithoutCancel(ctx), completed)
	}
	return Capture(ctx, sessionRecorder{s}, s.runID, identity, action)
}

type sessionRecorder struct{ session *Session }

func (r sessionRecorder) RecordSemanticMutation(ctx context.Context, receipt Receipt) error {
	return r.session.record(ctx, receipt)
}

func (s *Session) record(ctx context.Context, receipt Receipt) error {
	if err := s.recorder.RecordSemanticMutation(ctx, receipt); err != nil {
		return err
	}
	s.history = append(s.history, receipt)
	return nil
}

func matchingReceipts(history []Receipt, identity Identity) ([]Receipt, error) {
	byID := map[string]Receipt{}
	for _, receipt := range history {
		if err := receipt.Validate(); err != nil {
			return nil, err
		}
		if prior, ok := byID[receipt.ID]; ok {
			if prior.RunID != receipt.RunID || prior.Mutation != receipt.Mutation {
				return nil, fmt.Errorf("conflicting semantic mutation receipt identity")
			}
			if prior.Phase == "completed" {
				continue
			}
		}
		byID[receipt.ID] = receipt
	}
	var matches []Receipt
	for _, receipt := range byID {
		if receipt.Mutation == identity {
			matches = append(matches, receipt)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })
	return matches, nil
}

type freshReadKey struct{}

// FreshRead requires provider adapters and caches to fetch current evidence.
func FreshRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, freshReadKey{}, true)
}

// IsFreshRead reports whether a request must bypass cached provider snapshots.
func IsFreshRead(ctx context.Context) bool {
	fresh, _ := ctx.Value(freshReadKey{}).(bool)
	return fresh
}

// FreshReadHeader carries the evidence-read requirement through HTTP decorators.
const FreshReadHeader = "X-Goobers-Mutation-Fresh-Read"

func containsReceipt(receipts []Receipt, observed Receipt) bool {
	for _, receipt := range receipts {
		if receipt == observed {
			return true
		}
	}
	return false
}
