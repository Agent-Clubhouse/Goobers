package prstatus

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/goobers/goobers/providers"
)

// VerdictObservation contains closed diagnostic classes for an existing live
// poll. These fields are telemetry only and never authorize or refuse landing.
type VerdictObservation struct {
	Presence  string
	Reason    string
	Phase     string
	State     string
	BaseMatch string
}

// ObserveVerdict adds no provider calls. Only a complete native-status read can
// establish absence; status publishing support alone does not prove read support.
func ObserveVerdict(provider any, poll providers.PullRequestPollResult, pollErr error, expectedBase string) VerdictObservation {
	o := VerdictObservation{Presence: "unreadable", Reason: "response-incomplete", Phase: "checks", State: "none", BaseMatch: "not-applicable"}
	if pollErr != nil {
		o.Reason = observationErrorClass(pollErr)
		o.Phase = observationPhase(poll.PollErrorPhase)
		return o
	}
	if _, ok := provider.(providers.MergeReviewStatusReader); !ok {
		o.Presence, o.Reason = "unsupported", "reader-unsupported"
		return o
	}
	if poll.VerdictReadStatus != providers.VerdictReadComplete {
		return o
	}
	if poll.VerdictStatus == nil {
		o.Presence, o.Reason = "absent", "complete-read"
		return o
	}
	o.Presence, o.Reason = "present", "complete-read"
	switch poll.VerdictStatus.State {
	case providers.CheckStatePassing:
		o.State = "pass"
	case providers.CheckStateFailing:
		o.State = "fail"
	case providers.CheckStatePending:
		o.State = "pending"
	default:
		o.State = "unknown"
	}
	decision, base, ok := ParseVerdictDescription(poll.VerdictStatus.Summary)
	expectedState, err := VerdictState(decision)
	if decision == "stale" {
		expectedState, err = providers.CheckStatePending, nil
	}
	if !ok || base == "" || err != nil || expectedState != poll.VerdictStatus.State {
		o.Reason, o.BaseMatch = "malformed-verdict", "unparseable"
		return o
	}
	switch {
	case expectedBase == "":
		o.BaseMatch = "unknown"
	case base != expectedBase:
		o.BaseMatch = "stale"
	default:
		o.BaseMatch = "matched"
	}
	return o
}

func observationErrorClass(err error) string {
	var syntax *json.SyntaxError
	var wrongType *json.UnmarshalTypeError
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.As(err, &syntax), errors.As(err, &wrongType), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "decode"
	case providers.IsAuthenticationError(err):
		return "authentication"
	case providers.IsTransientError(err):
		return "transient"
	default:
		return "poll-error"
	}
}

func observationPhase(phase string) string {
	switch phase {
	case "request", "pull-request", "reviews", "checks", "comments":
		return phase
	default:
		return "unknown"
	}
}

// WouldBlockMissingVerdict excludes opt-outs, advisory runs, other gate
// refusals, and incomplete evidence. A landing attempt may still fail natively.
func (o VerdictObservation) WouldBlockMissingVerdict(landingAttempted bool) bool {
	return o.Presence == "absent" && landingAttempted
}
