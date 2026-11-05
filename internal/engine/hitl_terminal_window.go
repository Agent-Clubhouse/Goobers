package engine

// HITLRequestStateQuery resolves one API idempotency key without rebinding an
// old acknowledged request to the workflow's current terminal generation.
const HITLRequestStateQuery = "goobers.hitl-request-state.v1"

// HITLAcknowledgementWindow bounds both the new query map and its workflow
// ordering index. Older keys remain in the existing deduplication ledger.
const HITLAcknowledgementWindow = 64

func (s *hitlSession) rememberTerminalAck(id string, ack HITLAck) {
	_, seen := s.acks[id]
	s.acks[id] = ack
	if !s.liveTerminal || seen {
		return
	}
	s.recentAcks = append(s.recentAcks, id)
	if len(s.recentAcks) > HITLAcknowledgementWindow {
		s.recentAcks = append([]string(nil), s.recentAcks[len(s.recentAcks)-HITLAcknowledgementWindow:]...)
	}
}

func (s *hitlSession) requestState(id string) (HITLState, error) {
	state, err := s.state()
	if err != nil {
		return state, err
	}
	if ack, seen := s.acks[id]; seen {
		state.RequestKnown = true
		if _, recent := state.AcknowledgedGenerations[id]; recent {
			state.RequestGeneration = ack.TerminalGeneration
		} else {
			state.RequestExpired = true
		}
	} else if s.delivery != nil && s.delivery.request.Transition.RequestID == id {
		state.RequestKnown = true
		state.RequestGeneration = s.delivery.request.Transition.ExpectedTerminalGeneration
	}
	return state, nil
}
