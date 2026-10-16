package prqueue

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testReport() Report {
	r := Report{Version: 1, RepositoryKey: "github|||org|repo|", Workflow: "review", RunID: "run", ObservedAt: time.Now().UTC(), Items: []Item{}}
	r.Add(42, Escalated)
	return r
}

func TestReportEncodingRoundTripAndFailClosed(t *testing.T) {
	r := testReport()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Items[0].Reason != Escalated || decoded.Items[0].Claim.State != "unknown" {
		t.Fatalf("lost evidence: %+v", decoded)
	}
	for _, bad := range []string{
		`null`, `{}`, string(data) + `{}`,
		strings.Replace(string(data), `"version":1`, `"version":2`, 1),
		strings.Replace(string(data), `"number":42`, `"number":0`, 1),
		strings.Replace(string(data), `"eligible":false`, `"eligible":true`, 1),
		strings.Replace(string(data), `"state":"unknown"`, `"state":"unrecognized"`, 1),
		strings.Replace(string(data), `"number":42`, `"extra":true,"number":42`, 1),
		strings.Replace(string(data), `"eligible":false,`, ``, 1),
		strings.Replace(string(data), `"completeSnapshot":false,`, ``, 1),
		strings.Replace(string(data), `"providerClaimLabel":false`, `"providerClaimLabel":null`, 1),
		strings.Replace(string(data), `"comparison":"unavailable"`, `"comparison":"no-local-lease-or-provider-label"`, 1),
		strings.Repeat(" ", MaxReportBytes+1),
	} {
		if err := decoded.UnmarshalJSON([]byte(bad)); err == nil {
			t.Fatal("accepted invalid report")
		}
	}
}

func TestReportProducerRejectsUnboundedAndContradictoryEvidence(t *testing.T) {
	for _, mutate := range []func(*Report){
		func(r *Report) { r.RunID = strings.Repeat("x", 257) },
		func(r *Report) { r.OmittedItems = -1 },
		func(r *Report) { r.Items = append(r.Items, r.Items[0]); r.MatchingItems++ },
		func(r *Report) { r.Items[0].Claim.OwnerRunID = "invented" },
		func(r *Report) { r.Items[0].NextStep = strings.Repeat("x", 2049) },
	} {
		r := testReport()
		mutate(&r)
		if _, err := json.Marshal(r); err == nil {
			t.Fatal("producer accepted invalid report")
		}
	}
}

func TestMarshalBoundedChecksErrorBeforeSizeLimit(t *testing.T) {
	atLimit := strings.Repeat("x", MaxReportBytes-2)
	if data, err := marshalBounded(atLimit); err != nil || len(data) != MaxReportBytes {
		t.Fatalf("at limit: len=%d err=%v", len(data), err)
	}
	if _, err := marshalBounded(atLimit + "x"); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("over limit: %v", err)
	}
	if _, err := marshalBounded(make(chan int)); err == nil || strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("marshal error not surfaced: %v", err)
	}
}

func TestValidateClaimRejectionsAndLegacyOverride(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	report := Report{RunID: "run", ObservedAt: now, Gaggle: "g"}
	valid := func(mut func(*ClaimObservation)) ClaimObservation {
		c := ObserveClaim(true, "other", "run", future, now, true)
		mut(&c)
		return c
	}
	tests := []struct {
		name string
		c    ClaimObservation
		want string
	}{
		{"owner too long", valid(func(c *ClaimObservation) { c.OwnerRunID = strings.Repeat("x", 257) }), "field limits"},
		{"missing next step", valid(func(c *ClaimObservation) { c.NextStep = "" }), "field limits"},
		{"next step too long", valid(func(c *ClaimObservation) { c.NextStep = strings.Repeat("x", 2049) }), "field limits"},
		{"unclaimed with owner", ClaimObservation{State: "unclaimed", OwnerRunID: "o", Comparison: "no-local-lease-or-provider-label", NextStep: "n"}, "has an owner"},
		{"unknown with expiry", ClaimObservation{State: "unknown", ExpiresAt: &future, Comparison: "unavailable", NextStep: "n"}, "has an owner"},
		{"held without owner", valid(func(c *ClaimObservation) { c.OwnerRunID = "" }), "lacks owner or expiry"},
		{"held without expiry", valid(func(c *ClaimObservation) { c.ExpiresAt = nil }), "lacks owner or expiry"},
		{"unknown state", valid(func(c *ClaimObservation) { c.State = "bogus" }), "unknown claim observation state"},
		{"unknown comparison", valid(func(c *ClaimObservation) { c.Comparison = "bogus" }), "unknown claim comparison"},
		{"state contradicts expiry", valid(func(c *ClaimObservation) { c.ExpiresAt = &past }), "contradicts"},
		{"comparison contradicts label", valid(func(c *ClaimObservation) { c.Comparison = "unavailable" }), "contradicts"},
		{"legacy without live lease", ClaimObservation{State: "held-in-legacy-namespace", OwnerRunID: "o", ExpiresAt: &past, Comparison: "provider-label-without-live-local-lease", ProviderClaimLabel: true, NextStep: "n"}, "contradicts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateClaim(tt.c, report)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want error containing %q", err, tt.want)
			}
		})
	}

	legacy := valid(func(c *ClaimObservation) { c.State = "held-in-legacy-namespace" })
	if err := validateClaim(legacy, report); err != nil {
		t.Fatalf("legacy override rejected: %v", err)
	}
	if err := validateClaim(legacy, Report{RunID: "run", ObservedAt: now}); err == nil {
		t.Fatal("legacy override accepted without gaggle")
	}
}
