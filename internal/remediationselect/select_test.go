package remediationselect

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/providers"
)

func TestRunBranchOriginRunIDOnlyReadsTheRunScopedShape(t *testing.T) {
	const run = "0123456789abcdef0123456789abcdef"
	for head, want := range map[string]string{
		"goobers/implementation/" + run:                           run,
		"team/sub/implementation/" + run:                          run,
		"goobers/" + run:                                          "",
		"goobers/implementation/42":                               "",
		"goobers/implementation/0123456789ABCDEF0123456789ABCDEF": "",
	} {
		if got := RunBranchOriginRunID(head); got != want {
			t.Errorf("RunBranchOriginRunID(%q) = %q, want %q", head, got, want)
		}
	}
}

func TestPullRequestClaimStatusFollowsTheLeaseAndItsOwner(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	live := claimsclient.Entry{Gaggle: "g", Provider: string(providers.ProviderGitHub), ExternalID: PullRequestClaimKey(7), ItemID: PullRequestClaimKey(7), RunID: "run-a", ExpiresAt: now.Add(time.Hour)}
	expired := live
	expired.ExpiresAt = now.Add(-time.Minute)
	for name, tc := range map[string]struct {
		entry         claimsclient.Entry
		run           string
		claimed, mine bool
	}{
		"live, another run":  {live, "run-b", true, false},
		"live, the same run": {live, "run-a", true, true},
		"live, no run":       {live, "", true, false},
		"expired":            {expired, "run-a", false, false},
		"no claim at all":    {claimsclient.Entry{}, "run-a", false, false},
	} {
		listing := claimsclient.Listing{Entries: []claimsclient.Entry{tc.entry}}
		claimed, mine := PullRequestClaimStatus(listing, "g", providers.ProviderGitHub, 7, tc.run, now)
		if claimed != tc.claimed || mine != tc.mine {
			t.Errorf("%s: claimed=%v mine=%v, want %v %v", name, claimed, mine, tc.claimed, tc.mine)
		}
	}
}

func TestBlockedOnSiblingRecordRoundTripsThroughItsComment(t *testing.T) {
	want := BlockedOnSiblingState{Blockers: []int{3, 9}, Reason: "waiting", HeadSHA: "h", BaseSHA: "b"}
	body := "<!-- blocked-on-sibling: {\"blockers\":[3,9],\"reason\":\"waiting\",\"headSha\":\"h\",\"baseSha\":\"b\"} -->"
	got, ok := ParseBlockedOnSiblingComment(body)
	if !ok || got.Reason != want.Reason || len(got.Blockers) != 2 || got.Blockers[0] != 3 || got.Blockers[1] != 9 {
		t.Fatalf("parsed %+v ok=%v", got, ok)
	}
	if _, ok := ParseBlockedOnSiblingComment("an ordinary comment"); ok {
		t.Fatal("an ordinary comment parsed as a record")
	}
}
