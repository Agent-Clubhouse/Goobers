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

func TestValidateClaimTable(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	report := Report{RunID: "me", Gaggle: "g", ObservedAt: now}
	other := report
	other.Gaggle = ""
	valid := func(owner string, exp time.Time, label bool) ClaimObservation {
		return ObserveClaim(true, owner, "me", exp, now, label)
	}
	mut := func(c ClaimObservation, f func(*ClaimObservation)) ClaimObservation {
		f(&c)
		return c
	}
	long := strings.Repeat("x", 3000)
	tests := []struct {
		name    string
		claim   ClaimObservation
		report  Report
		wantErr string
	}{
		{"unknown ok", ObserveClaim(false, "", "me", now, now, false), report, ""},
		{"unclaimed ok", valid("", now, false), report, ""},
		{"held-by-this-run ok", valid("me", future, true), report, ""},
		{"held-by-other-run ok", valid("o", future, false), report, ""},
		{"expired ok", valid("o", past, true), report, ""},
		{"legacy override ok", mut(valid("o", future, true), func(c *ClaimObservation) { c.State = "held-in-legacy-namespace" }), report, ""},
		{"legacy this-run override ok", mut(valid("me", future, true), func(c *ClaimObservation) { c.State = "held-in-legacy-namespace" }), report, ""},
		{"legacy without gaggle", mut(valid("o", future, true), func(c *ClaimObservation) { c.State = "held-in-legacy-namespace" }), other, "contradicts"},
		{"owner too long", mut(valid("o", future, true), func(c *ClaimObservation) { c.OwnerRunID = long }), report, "field limits"},
		{"missing next step", mut(valid("o", future, true), func(c *ClaimObservation) { c.NextStep = "" }), report, "field limits"},
		{"next step too long", mut(valid("o", future, true), func(c *ClaimObservation) { c.NextStep = long }), report, "field limits"},
		{"unclaimed with owner", mut(valid("", now, false), func(c *ClaimObservation) { c.OwnerRunID = "o" }), report, "has an owner"},
		{"unknown with expiry", mut(valid("", now, false), func(c *ClaimObservation) { c.State = "unknown"; c.ExpiresAt = &now }), report, "has an owner"},
		{"held lacks owner", mut(valid("o", future, true), func(c *ClaimObservation) { c.OwnerRunID = "" }), report, "lacks owner or expiry"},
		{"held lacks expiry", mut(valid("o", future, true), func(c *ClaimObservation) { c.ExpiresAt = nil }), report, "lacks owner or expiry"},
		{"expired lacks owner", mut(valid("o", past, true), func(c *ClaimObservation) { c.OwnerRunID = "" }), report, "lacks owner or expiry"},
		{"unknown state", mut(valid("o", future, true), func(c *ClaimObservation) { c.State = "bogus" }), report, "unknown claim observation state"},
		{"unknown comparison", mut(valid("o", future, true), func(c *ClaimObservation) { c.Comparison = "bogus" }), report, "unknown claim comparison"},
		{"state contradicts expiry", mut(valid("o", past, true), func(c *ClaimObservation) { c.State = "held-by-other-run" }), report, "contradicts"},
		{"comparison contradicts label", mut(valid("o", future, true), func(c *ClaimObservation) { c.Comparison = "local-lease-without-provider-label" }), report, "contradicts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateClaim(tt.claim, tt.report)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
