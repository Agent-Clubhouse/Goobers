package runner

import (
	"fmt"
	"sync"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// OperatorMessageDeliveryRegistry tracks live local agent targets by their
// portable journal address.
type OperatorMessageDeliveryRegistry struct {
	mu           sync.Mutex
	targets      map[string]invoke.OperatorMessageTarget
	visitTargets map[string]map[string]invoke.OperatorMessageTarget
}

// DefaultOperatorMessageDeliveryRegistry is shared by the in-process local
// runner and daemon write API.
var DefaultOperatorMessageDeliveryRegistry = &OperatorMessageDeliveryRegistry{}

// Register publishes a live target until the returned cleanup function runs.
func (r *OperatorMessageDeliveryRegistry) Register(address string, target invoke.OperatorMessageTarget) func() {
	if r == nil || address == "" || target == nil {
		return func() {}
	}
	r.mu.Lock()
	if r.targets == nil {
		r.targets = make(map[string]invoke.OperatorMessageTarget)
	}
	if r.visitTargets == nil {
		r.visitTargets = make(map[string]map[string]invoke.OperatorMessageTarget)
	}
	r.targets[address] = target
	visitKey := operatorMessageVisitKey(address)
	if visitKey != "" {
		if r.visitTargets[visitKey] == nil {
			r.visitTargets[visitKey] = make(map[string]invoke.OperatorMessageTarget)
		}
		r.visitTargets[visitKey][address] = target
	}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if r.targets[address] == target {
			delete(r.targets, address)
		}
		if visitKey != "" && r.visitTargets[visitKey][address] == target {
			delete(r.visitTargets[visitKey], address)
			if len(r.visitTargets[visitKey]) == 0 {
				delete(r.visitTargets, visitKey)
			}
		}
		r.mu.Unlock()
	}
}

// Resolve returns the currently live target for address, when this process owns
// it.
func (r *OperatorMessageDeliveryRegistry) Resolve(address string) (invoke.OperatorMessageTarget, bool) {
	if r == nil || address == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if target, ok := r.targets[address]; ok {
		return target, true
	}
	return nil, false
}

// ResolveVisit returns the unique live target for the same run/stage/attempt
// visit as address. It is used after the journal resolver has already proven
// that the selected address is live in that visit.
func (r *OperatorMessageDeliveryRegistry) ResolveVisit(address string) (invoke.OperatorMessageTarget, bool) {
	if r == nil || address == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	visitTargets := r.visitTargets[operatorMessageVisitKey(address)]
	if len(visitTargets) != 1 {
		return nil, false
	}
	for _, target := range visitTargets {
		return target, true
	}
	return nil, false
}

func operatorMessageVisitKey(raw string) string {
	address, err := journal.ParseAgentAddress(raw)
	if err != nil {
		return ""
	}
	startedSeq, err := address.StageStartedSeq()
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s\x00%s\x00%d\x00%d", address.RunID, address.Stage, address.Attempt, startedSeq)
}
