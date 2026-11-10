package decisiongate

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decider"
)

func scopeGate(t *testing.T, f *fake, timeout time.Duration) *Gate {
	t.Helper()
	cfg := Config{Thresholds: map[string]Threshold{ScopeThresholdKey: DefaultScopeThreshold}, CallTimeout: timeout}
	g, err := New(f, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

type hangingDecider struct{}

func (hangingDecider) Decide(ctx context.Context, _ decider.Request) (decider.Response, error) {
	<-ctx.Done()
	return decider.Response{}, ctx.Err()
}

func TestScopeCodeFactsSkipModel(t *testing.T) {
	f := &fake{yes: 0.0}
	in := ScopeInput{
		DiffFiles: []string{"internal/a/a.go"},
		Findings: []ScopeFinding{
			{Severity: "critical", File: "other/x.go", Text: "boom"},
			{Severity: "warning", Category: "rebase-needed", File: "internal/a/a.go", Text: "base moved"},
			{Severity: "error", Category: "cross-pr-blocked", Text: "waits on #3"},
			{Severity: "error", File: "internal/a/a.go:42", Text: "leaks"},
			{Severity: "error", Text: "see internal/a/a.go for the race"},
		},
	}
	recs := scopeGate(t, f, 0).ScopeCheck(context.Background(), in)
	want := []ScopeOutcome{ScopeInScope, ScopeWouldDemote, ScopeWouldDemote, ScopeInScope, ScopeInScope}
	for i, w := range want {
		if recs[i].Outcome != w || recs[i].Source != ScopeByCode {
			t.Errorf("finding %d: %s by %s, want %s by code", i, recs[i].Outcome, recs[i].Source, w)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("model called %d times for code-decided findings", n)
	}
}

func TestScopeFindingFromAPIStripsLine(t *testing.T) {
	got := ScopeFindingFromAPI(apiv1.Finding{Severity: apiv1.SeverityWarning, Class: apiv1.FindingRebaseNeeded, Location: "internal/a/a.go:42", Message: "m"})
	if got.File != "internal/a/a.go" || got.Category != "rebase-needed" || got.Severity != "warning" {
		t.Fatalf("got %+v", got)
	}
}

func TestScopeUnchangedFromPriorAndOutsideDiff(t *testing.T) {
	f := ScopeFinding{Severity: "error", File: "docs/x.md", Text: "Rewrite  THIS section"}
	prior := []ScopeFinding{{Severity: "warning", File: "./docs/x.md", Text: "rewrite this section"}}
	facts := ScopeFactsOf(f, []string{"internal/a/a.go"}, prior)
	if !facts.UnchangedFromPrior || !facts.OutsideDiff || facts.NamesDiffFile {
		t.Fatalf("facts = %+v", facts)
	}
}

func TestScopeModelThresholds(t *testing.T) {
	in := ScopeInput{Criteria: "add the flag", DiffFiles: []string{"a.go"}, Findings: []ScopeFinding{{Severity: "error", File: "b.go", Text: "rename the package"}}}
	for _, tc := range []struct {
		yes  float64
		want ScopeOutcome
	}{{0.95, ScopeInScope}, {0.05, ScopeWouldDemote}, {0.5, ScopeUnsure}} {
		recs := scopeGate(t, &fake{yes: tc.yes}, 0).ScopeCheck(context.Background(), in)
		if recs[0].Outcome != tc.want || recs[0].Source != ScopeByModel || recs[0].Probability != tc.yes {
			t.Errorf("yes=%v: %+v, want %s", tc.yes, recs[0], tc.want)
		}
	}
}

func TestScopeFallsBackOnModelErrorAndTimeout(t *testing.T) {
	in := ScopeInput{Findings: []ScopeFinding{{Severity: "error", Text: "unscoped concern"}}}
	recs := scopeGate(t, &fake{err: errors.New("down")}, 0).ScopeCheck(context.Background(), in)
	if recs[0].Outcome != ScopeUnsure || recs[0].Error == "" {
		t.Fatalf("error case: %+v", recs[0])
	}
	g, err := New(hangingDecider{}, Config{Thresholds: map[string]Threshold{ScopeThresholdKey: DefaultScopeThreshold}, CallTimeout: 10 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	recs = g.ScopeCheck(context.Background(), in)
	if recs[0].Outcome != ScopeUnsure || recs[0].Error == "" {
		t.Fatalf("timeout case: %+v", recs[0])
	}
}

func TestScopeCapsModelCalls(t *testing.T) {
	f := &fake{yes: 0.01}
	in := ScopeInput{}
	for i := 0; i < MaxScopeModelCalls+5; i++ {
		in.Findings = append(in.Findings, ScopeFinding{Severity: "error", Text: strings.Repeat("x", i+1)})
	}
	recs := scopeGate(t, f, 0).ScopeCheck(context.Background(), in)
	if int(f.calls.Load()) != MaxScopeModelCalls || recs[len(recs)-1].Outcome != ScopeUnsure {
		t.Fatalf("calls=%d last=%+v", f.calls.Load(), recs[len(recs)-1])
	}
}

func TestScopeRecordsOmitTextAndReportRefusesSmallSamples(t *testing.T) {
	secret := "SECRET-FINDING-TEXT"
	recs := scopeGate(t, &fake{yes: 0.01}, 0).ScopeCheck(context.Background(),
		ScopeInput{Criteria: "SECRET-CRITERIA", Findings: []ScopeFinding{{Severity: "error", Text: secret}}})
	recs[0].Reference = ScopeInScope
	var buf bytes.Buffer
	if err := WriteScopeRecords(&buf, recs); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "SECRET") {
		t.Fatalf("record leaks text: %s", buf.String())
	}
	back, err := ReadScopeRecords(&buf)
	if err != nil || len(back) != 1 {
		t.Fatalf("round trip: %v %v", back, err)
	}
	tally := NewScopeTally(back)
	if tally.WouldDemote != 1 || tally.WouldDemoteInScope != 1 || tally.Labeled != 1 {
		t.Fatalf("tally = %+v", tally)
	}
	out := tally.Render(0)
	if strings.Contains(out, "%") || !strings.Contains(out, "would-demote=1") || !strings.Contains(out, "n/a (n=1<30)") {
		t.Fatalf("report:\n%s", out)
	}
	if !strings.Contains(tally.Render(1), "100.0% (1/1)") {
		t.Fatalf("report at min 1:\n%s", tally.Render(1))
	}
}
