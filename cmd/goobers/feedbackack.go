package main

import (
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Snapshot-bound feedback acknowledgement (#6918).
//
// A Goobers response that answers a remediation run's feedback carries a
// feedback-ack marker naming the run's acknowledgement frontier: the newest
// timestamp among the comments and reviews the run's feedback snapshot read
// (second precision, UTC). pr-comment-watch treats such a response as
// acknowledging only human comments at or before that frontier, so a reply to
// older feedback never masks feedback that landed after the snapshot. "none"
// means the snapshot held no timestamped feedback: the response acknowledges
// no comment at all. A response without a feedback snapshot carries no marker
// and keeps the legacy rule (it acknowledges everything before itself).
const (
	feedbackAckMarkerPrefix = "<!-- goobers:feedback-ack:"
	feedbackAckMarkerSuffix = " -->"
	feedbackAckNone         = "none"
)

// feedbackAckMarker renders the marker for frontier; a zero frontier is none.
func feedbackAckMarker(frontier time.Time) string {
	value := feedbackAckNone
	if !frontier.IsZero() {
		value = frontier.UTC().Format(time.RFC3339)
	}
	return feedbackAckMarkerPrefix + value + feedbackAckMarkerSuffix
}

// remediationFeedbackAck is the marker a response to brief's feedback carries,
// or "" when the brief pins no feedback snapshot (nothing bounds what was
// assessed, so the response stays unbound).
func remediationFeedbackAck(brief apiv1.RemediationBrief) string {
	if brief.FeedbackSnapshot == nil {
		return ""
	}
	var stamps []string
	for _, c := range brief.GatherPRContext.Comments {
		stamps = append(stamps, c.CreatedAt)
	}
	if threads := brief.GatherReviewThreads; threads != nil {
		for _, c := range threads.InlineComments {
			stamps = append(stamps, c.CreatedAt)
		}
		for _, r := range threads.Reviews {
			stamps = append(stamps, r.SubmittedAt)
		}
	}
	var frontier time.Time
	for _, stamp := range stamps {
		if at, err := time.Parse(time.RFC3339, stamp); err == nil && at.After(frontier) {
			frontier = at
		}
	}
	return feedbackAckMarker(frontier.Truncate(time.Second))
}

// withFeedbackAck appends ack on a line of its own; an empty ack is a no-op.
func withFeedbackAck(body, ack string) string {
	if ack == "" {
		return body
	}
	return strings.TrimRight(body, "\n") + "\n\n" + ack
}

// parseFeedbackAck returns the frontier of body's last unquoted feedback-ack
// marker line and whether one is present. A present but unreadable marker, or
// none, yields a zero frontier: incomplete acknowledgement evidence covers no
// comment rather than silently covering every earlier one.
func parseFeedbackAck(body string) (time.Time, bool) {
	value, found := "", false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, feedbackAckMarkerPrefix) && strings.HasSuffix(trimmed, feedbackAckMarkerSuffix) {
			value = strings.TrimSuffix(strings.TrimPrefix(trimmed, feedbackAckMarkerPrefix), feedbackAckMarkerSuffix)
			found = true
		}
	}
	if !found {
		return time.Time{}, false
	}
	frontier, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, true
	}
	return frontier, true
}
