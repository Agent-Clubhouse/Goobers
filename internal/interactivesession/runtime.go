package interactivesession

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Observation is verified host journal and process custody evidence. Neither
// the agent's text nor its authored Result can set Terminal or WritersJoined.
// Absent means the host proved the expected journal directory does not exist;
// unreadable, partially created and foreign journals are errors, never absence.
type Observation struct {
	Found, Absent, Terminal, WritersJoined bool
	Identity                               journal.RunIdentity
	Outcome, Text                          string
}

// PreparedTurn contains immutable compiled source and its generation pin.
// Build performs no process/provider/model effects. Run owns all effects and
// calls published after the actual journal exists, before starting a process.
// Release is called only after no effects or verified joined execution.
type PreparedTurn struct {
	Identity journal.RunIdentity
	Run      func(context.Context, *interactiveaccess.ExecutionLease, func() error) error
	Release  func()
}

// Runtime connects the queue to a real execution backend. Reserve/Restore use
// shared scheduler capacity; Observe independently verifies the real journal.
// Installation is host-only and completed before serving or sweeping requests.
type Runtime struct {
	Build            func(context.Context, triggerqueue.SessionTurn, sessioning.ExecutionInputs) (PreparedTurn, error)
	Observe          func(context.Context, triggerqueue.SessionTurn, sessioning.ExecutionInputs) (Observation, error)
	Reserve          func(context.Context, journal.RunIdentity, time.Time) (func(), error)
	Restore          func(journal.RunIdentity) (func(), error)
	RegisterDispatch func() func()
}

type executionOwner struct {
	cancel                         context.CancelFunc
	lease                          *interactiveaccess.ExecutionLease
	releaseCapacity, releaseSource func()
	done                           bool
}
type executionState struct {
	mu      sync.Mutex
	owners  map[string]*executionOwner
	workers sync.WaitGroup
}

// Wait joins all host execution calls after the daemon execution context has
// been canceled. Unknown writer evidence remains durably unsettled afterward.
func (s *Service) Wait() { s.execution.workers.Wait() }

func (s *Service) runtimeReady() bool {
	return s.Runtime != nil && s.Runtime.Build != nil && s.Runtime.Observe != nil && s.Runtime.Reserve != nil && s.Runtime.Restore != nil
}

func verifyTurnIdentity(t triggerqueue.SessionTurn, inputs sessioning.ExecutionInputs, id journal.RunIdentity) error {
	raw, err := inputs.Validate(strings.TrimPrefix(t.Record.ID, "trigger-"), t.Session.Gaggle)
	if err != nil {
		return err
	}
	expected := &journal.SessionLineage{Gaggle: t.Session.Gaggle, SessionID: t.Session.ID, TurnID: t.ID, MessageID: t.Message.ID, AcceptanceID: t.Record.ID, EnvelopeDigest: journal.Digest(t.Record.Payload), InputDigest: journal.Digest(raw)}
	if id.RunID != strings.TrimPrefix(t.Record.ID, "trigger-") || id.Gaggle != t.Session.Gaggle || id.ConfigGeneration != t.Session.ConfigGeneration || id.GooberDigest != t.Session.GooberDigest || !reflect.DeepEqual(id.Session, expected) || id.ValidateSessionLineage() != nil {
		return errors.New("interactive session journal provenance mismatch")
	}
	return nil
}

func (s *Service) readObservation(ctx context.Context, t triggerqueue.SessionTurn, inputs sessioning.ExecutionInputs) (Observation, error) {
	observed, err := s.Runtime.Observe(ctx, t, inputs)
	if err != nil {
		return Observation{}, err
	}
	if observed.Found {
		if observed.Absent {
			return Observation{}, errors.New("interactive session contradictory custody evidence")
		}
		if err = verifyTurnIdentity(t, inputs, observed.Identity); err != nil {
			return Observation{}, err
		}
	} else if observed.Terminal || observed.WritersJoined {
		return Observation{}, errors.New("interactive session evidence has no verified journal")
	}
	return observed, nil
}

func releaseOwner(o *executionOwner) {
	if o == nil {
		return
	}
	if o.cancel != nil {
		o.cancel()
	}
	if o.releaseSource != nil {
		o.releaseSource()
	}
	if o.lease != nil {
		o.lease.Close()
	}
	if o.releaseCapacity != nil {
		o.releaseCapacity()
	}
}
