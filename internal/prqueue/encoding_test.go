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
