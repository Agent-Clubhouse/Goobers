package runner

import (
	"fmt"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// OperatorMessageJournal is the live journal surface needed to durably record
// operator-message lifecycle transitions while the owning local runner still
// holds the run lock.
type OperatorMessageJournal interface {
	AcceptOperatorMessage(apiv1.OperatorMessageRequest) (apiv1.OperatorMessageRecord, bool, error)
	CompleteOperatorMessage(apiv1.OperatorMessageOutcome) (apiv1.OperatorMessageRecord, error)
}

// OperatorMessageDeliveryRegistry tracks live local agent targets by their
// portable journal address.
type OperatorMessageDeliveryRegistry struct {
	mu            sync.Mutex
	targets       map[string]invoke.OperatorMessageTarget
	visitTargets  map[string]map[string]invoke.OperatorMessageTarget
	journals      map[string]OperatorMessageJournal
	visitJournals map[string]map[string]OperatorMessageJournal
}

// DefaultOperatorMessageDeliveryRegistry is shared by the in-process local
// runner and daemon write API.
var DefaultOperatorMessageDeliveryRegistry = &OperatorMessageDeliveryRegistry{}

// RegisterWithJournal publishes a live target and, when supplied, the active
// run journal that owns it.
func (r *OperatorMessageDeliveryRegistry) RegisterWithJournal(address string, target invoke.OperatorMessageTarget, run OperatorMessageJournal) func() {
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
	if r.journals == nil {
		r.journals = make(map[string]OperatorMessageJournal)
	}
	if r.visitJournals == nil {
		r.visitJournals = make(map[string]map[string]OperatorMessageJournal)
	}
	r.targets[address] = target
	if run != nil {
		r.journals[address] = run
	}
	visitKey := operatorMessageVisitKey(address)
	if visitKey != "" {
		if r.visitTargets[visitKey] == nil {
			r.visitTargets[visitKey] = make(map[string]invoke.OperatorMessageTarget)
		}
		r.visitTargets[visitKey][address] = target
		if run != nil {
			if r.visitJournals[visitKey] == nil {
				r.visitJournals[visitKey] = make(map[string]OperatorMessageJournal)
			}
			r.visitJournals[visitKey][address] = run
		}
	}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if r.targets[address] == target {
			delete(r.targets, address)
		}
		if run != nil && r.journals[address] == run {
			delete(r.journals, address)
		}
		if visitKey != "" && r.visitTargets[visitKey][address] == target {
			delete(r.visitTargets[visitKey], address)
			if len(r.visitTargets[visitKey]) == 0 {
				delete(r.visitTargets, visitKey)
			}
		}
		if run != nil && visitKey != "" && r.visitJournals[visitKey][address] == run {
			delete(r.visitJournals[visitKey], address)
			if len(r.visitJournals[visitKey]) == 0 {
				delete(r.visitJournals, visitKey)
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

// ResolveJournal returns the active run journal for address, when this process
// owns it.
func (r *OperatorMessageDeliveryRegistry) ResolveJournal(address string) (OperatorMessageJournal, bool) {
	if r == nil || address == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.journals[address]
	return run, ok
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

// ResolveVisitJournal returns the unique active journal for the same
// run/stage/attempt visit as address.
func (r *OperatorMessageDeliveryRegistry) ResolveVisitJournal(address string) (OperatorMessageJournal, bool) {
	if r == nil || address == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	visitJournals := r.visitJournals[operatorMessageVisitKey(address)]
	if len(visitJournals) != 1 {
		return nil, false
	}
	for _, run := range visitJournals {
		return run, true
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
