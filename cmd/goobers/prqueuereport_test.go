package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/prqueue"
	"github.com/goobers/goobers/providers"
)

// Existing selection outputs remain strings. Only the explicitly named,
// versioned report extension may be an object; arbitrary objects still fail.
func decodePRSelectionTestResult(data []byte, target *map[string]string) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if extension, ok := raw["queueEligibility"]; ok {
		var report prqueue.Report
		if err := json.Unmarshal(extension, &report); err != nil {
			return err
		}
		if report.Version != 1 || report.MatchingItems != len(report.Items)+report.OmittedItems {
			return fmt.Errorf("invalid queue report version/counts: %+v", report)
		}
		delete(raw, "queueEligibility")
	}
	*target = make(map[string]string, len(raw))
	for key, value := range raw {
		var scalar string
		if err := json.Unmarshal(value, &scalar); err != nil {
			return fmt.Errorf("selection output %s: %w", key, err)
		}
		(*target)[key] = scalar
	}
	return nil
}

func TestQueueReportTestDecoderRetainsScalarContract(t *testing.T) {
	for _, data := range []string{
		`{"number":{"unexpected":42}}`,
		`{"queueEligibility":{"version":99}}`,
		`{"queueEligibility":{"version":1,"matchingItems":1,"items":[]}}`,
	} {
		var result map[string]string
		if err := decodePRSelectionTestResult([]byte(data), &result); err == nil {
			t.Fatalf("accepted invalid result contract: %s", data)
		}
	}
}

func TestQueueReportUsesSelectorsClaimNamespaceResolution(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                       string
		gaggle                     string
		provider                   providers.ProviderKind
		entryGaggle, entryProvider string
		live, wantFound, wantOwned bool
	}{
		{"unscoped", "", providers.ProviderGitHub, "", "", true, true, true},
		{"scoped", "team", providers.ProviderGitea, "team", "gitea", true, true, true},
		{"legacy namespace blocks same run", "team", providers.ProviderGitHub, "", "", true, true, false},
		{"pre-migration provider", "team", providers.ProviderGitea, "team", "github", true, true, true},
		{"other gaggle invisible", "team", providers.ProviderGitHub, "other", "github", true, false, false},
		{"other provider invisible", "team", providers.ProviderGitHub, "team", "gitea", true, false, false},
		{"expiry boundary", "team", providers.ProviderGitHub, "team", "github", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expiry := now
			if tc.live {
				expiry = now.Add(time.Minute)
			}
			listing := claimsclient.Listing{Entries: []claimsclient.Entry{{
				ItemID: "pr/42", ExternalID: "pr/42", Gaggle: tc.entryGaggle,
				Provider: tc.entryProvider, RunID: "current", ExpiresAt: expiry,
			}}}
			entry, found, comparable := resolvePullRequestClaim(listing, tc.gaggle, tc.provider, 42, now)
			claimed, owned := pullRequestClaimStatus(listing, tc.gaggle, tc.provider, 42, "current", now)
			if found != tc.wantFound || claimed != (tc.wantFound && tc.live) || owned != tc.wantOwned {
				t.Fatalf("found=%t claimed=%t owned=%t", found, claimed, owned)
			}
			if found && (entry.RunID != "current" || (comparable && tc.live) != tc.wantOwned) {
				t.Fatalf("report and selector ownership disagree: entry=%+v comparable=%t", entry, comparable)
			}
		})
	}
}
