package decisiongate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func severityGate(t *testing.T, f *fake) *Gate {
	t.Helper()
	g, err := New(f, Config{Thresholds: map[string]Threshold{SeverityThresholdKey: DefaultSeverityThreshold}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestEvaluateSeverityCodeFactsSkipModel(t *testing.T) {
	f := &fake{yes: 0.99}
	g := severityGate(t, f)
	for comment, want := range map[string]Severity{
		"nit: rename this":                  SeverityNit,
		"**Question:** why a map here?":     SeverityQuestion,
		"Blocking - this leaks the handle":  SeverityBlocking,
		"Why not reuse the existing cache?": SeverityQuestion,
		"":                                  SeverityUnsure,
	} {
		got, _, err := g.EvaluateSeverity(context.Background(), comment)
		if err != nil || got != want {
			t.Fatalf("%q = %q err=%v, want %q", comment, got, err, want)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatalf("model called %d times for code-decided comments", f.calls.Load())
	}
}

func TestEvaluateSeverityModelThresholds(t *testing.T) {
	for name, tc := range map[string]struct {
		yes  float64
		want Severity
	}{
		"blocking": {0.95, SeverityBlocking},
		"nit":      {0.05, SeverityNit},
		"unsure":   {0.5, SeverityUnsure},
	} {
		t.Run(name, func(t *testing.T) {
			got, _, err := severityGate(t, &fake{yes: tc.yes}).EvaluateSeverity(context.Background(), "This ignores the error from Close and can lose data.")
			if err != nil || got != tc.want {
				t.Fatalf("got %q err=%v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestEvaluateSeverityFallsBackOnModelError(t *testing.T) {
	got, _, err := severityGate(t, &fake{err: errors.New("down")}).EvaluateSeverity(context.Background(), strings.Repeat("x", 5000))
	if err == nil || got != SeverityUnsure {
		t.Fatalf("got %q err=%v, want unsure with error", got, err)
	}
}

func TestSeverityTallyRefusesSmallSamples(t *testing.T) {
	var tally SeverityTally
	g := severityGate(t, &fake{yes: 0.05})
	ctx := context.Background()
	tally.Add(g.ShadowSeverity(ctx, "consider a shorter name here", SeverityNit))
	tally.Add(g.ShadowSeverity(ctx, "this drops the lock early", SeverityBlocking))
	tally.Add(g.ShadowSeverity(ctx, "nit: spacing", SeverityNit))
	if tally.Total != 3 || tally.Coded != 1 || tally.Judged != 2 || tally.Agree != 1 || tally.MissedBlocking != 1 {
		t.Fatalf("tally = %+v", tally)
	}
	if _, ok := tally.Agreement(10); ok {
		t.Fatal("agreement reported below the minimum sample")
	}
	if p, ok := tally.Agreement(2); !ok || p != 0.5 {
		t.Fatalf("agreement = %v ok=%v", p, ok)
	}
}
