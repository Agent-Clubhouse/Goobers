package main

import (
	"testing"
	"time"
)

func TestConfigReloadStatusDistinguishesObservations(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, applied, observed, rejection, readError, want string
		watching                                            bool
	}{
		{name: "current", applied: "a", observed: "a", watching: true, want: "current"},
		{name: "explicit opt out", applied: "a", observed: "a", want: "not-watching"},
		{name: "not admitted", applied: "a", observed: "b", watching: true, want: "pending"},
		{name: "rejected", applied: "a", observed: "b", rejection: "invalid", watching: true, want: "rejected"},
		{name: "unreadable", applied: "a", observed: "b", rejection: "invalid", readError: "permission denied", watching: true, want: "unreadable"},
		{name: "recovered", applied: "b", observed: "b", rejection: "previous failure", watching: true, want: "current"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &configReloader{appliedDigest: tc.applied, observedDigest: tc.observed, lastRejectionMessage: tc.rejection, lastDigestError: tc.readError, watching: tc.watching}
			if tc.rejection != "" {
				r.rejectedDigest = tc.observed
			}
			// Repeated manual apply clears only its response message. Health
			// must retain the rejection of the unchanged observed revision.
			r.lastRejectionMessage = ""
			got := r.reloadStatus(now)
			if got.State != tc.want || got.AppliedDigest != tc.applied || got.ObservedDigest != tc.observed || got.Watching != tc.watching || !got.ObservedAt.Equal(now) {
				t.Fatalf("reload observation: %+v, want state %s", got, tc.want)
			}
		})
	}
}
