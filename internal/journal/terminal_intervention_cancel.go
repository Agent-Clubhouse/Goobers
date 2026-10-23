package journal

import "strconv"

const interventionCancelled = "intervention.cancelled"

// WriteEvents returns the bounded transition this request may commit. A cancel
// retains Events solely to bind the original intent, never to reopen it.
func (r TerminalIntervention) WriteEvents() []Event {
	if len(r.Cancellation) > 0 {
		return r.Cancellation
	}
	return r.Events
}

func validateInterventionCancellation(req TerminalIntervention) error {
	if len(req.Cancellation) == 0 {
		return nil
	}
	if len(req.Cancellation) != 2 {
		return ErrInterventionInvalid
	}
	marker, abort := req.Cancellation[0], req.Cancellation[1]
	if marker.Type != EventRunnerAnnotation || marker.Runner["kind"] != interventionCancelled || !cancelBindingMatches(marker, req) || abort.Type != EventRunFinished || abort.Status != string(PhaseAborted) {
		return ErrInterventionInvalid
	}
	keys := map[string]bool{}
	for _, event := range req.Events {
		key, _ := event.Runner["emitKey"].(string)
		keys[key] = true
	}
	for _, event := range req.Cancellation {
		key, _ := event.Runner["emitKey"].(string)
		if key == "" || keys[key] || event.Branch != 0 {
			return ErrInterventionInvalid
		}
		keys[key] = true
	}
	return nil
}

func cancelBindingMatches(event Event, req TerminalIntervention) bool {
	return event.Runner["idempotencyKey"] == req.RequestID && event.Runner["fingerprint"] == req.Fingerprint && event.Runner["terminalGeneration"] == strconv.FormatUint(req.ExpectedTerminalGeneration, 10) && event.Runner["terminalSeq"] == strconv.FormatUint(req.ExpectedTerminalSeq, 10)
}

// observeCancellation intercepts tombstones before original-prefix matching.
// It permits cancellation after a file's marker-only prefix; the cancelled
// prefix must never be completed by a late normal retry.
func (c *TerminalInterventionCheck) observeCancellation(event Event) bool {
	if event.Type == EventRunnerAnnotation && event.Runner["kind"] == interventionCancelled && event.Runner["idempotencyKey"] == c.request.RequestID {
		if c.cancelMatched != 0 || !cancelBindingMatches(event, c.request) || c.generation != c.request.ExpectedTerminalGeneration || c.terminalSeq != c.request.ExpectedTerminalSeq {
			c.err = ErrInterventionConflict
			return true
		}
		if len(c.request.Cancellation) > 0 && !sameInterventionEvent(event, c.request.Cancellation[0]) {
			c.err = ErrInterventionConflict
			return true
		}
		c.cancelMatched = 1
		c.pending = ""
		c.cancelReceipt = c.cancellationReceipt()
		c.cancelReceipt.Seq = event.Seq
		return true
	}
	if c.cancelMatched > 0 {
		if len(c.request.Cancellation) > 0 && c.cancelMatched == 1 {
			if !sameInterventionEvent(event, c.request.Cancellation[1]) {
				c.err = ErrInterventionConflict
				return true
			}
			c.cancelMatched = 2
			c.cancelReceipt.Seq = event.Seq
		}
		// A cancelled request never needs to fold subsequent segments: either it
		// is refused, or it returns its immutable completed cancellation receipt.
		return true
	}
	if event.Type == EventRunnerAnnotation && event.Runner["kind"] == interventionCancelled {
		// A different intent's cancellation fence also closes the append lane
		// while its file prefix is incomplete.
		c.terminal = false
		c.pending = "cancellation in progress"
		return true
	}
	for _, expected := range c.request.Cancellation {
		if event.Runner["emitKey"] == expected.Runner["emitKey"] && !sameInterventionEvent(event, expected) {
			c.err = ErrInterventionConflict
			return true
		}
	}
	return false
}

func (c *TerminalInterventionCheck) cancellationReceipt() TerminalInterventionReceipt {
	resumed := len(c.request.Events) == 2 && c.matched == 2
	segment := c.generation
	if resumed {
		segment++
	}
	return TerminalInterventionReceipt{Segment: segment, Resumed: resumed, Cancelled: true}
}

func (c *TerminalInterventionCheck) remainingCancellation() (int, TerminalInterventionReceipt, error) {
	if len(c.request.Cancellation) == 0 {
		return 0, TerminalInterventionReceipt{}, ErrInterventionCancelled
	}
	if c.cancelMatched > 0 {
		receipt := c.cancelReceipt
		receipt.Duplicate = c.cancelMatched == 2
		return c.cancelMatched, receipt, nil
	}
	if c.pending != "" && c.pending != c.request.RequestID {
		return 0, TerminalInterventionReceipt{}, ErrInterventionPending
	}
	completed := c.matched == len(c.request.Events)
	if c.generation != c.request.ExpectedTerminalGeneration || c.terminalSeq != c.request.ExpectedTerminalSeq || (!c.terminal && !completed) {
		return 0, TerminalInterventionReceipt{}, ErrInterventionStale
	}
	return 0, c.cancellationReceipt(), nil
}
