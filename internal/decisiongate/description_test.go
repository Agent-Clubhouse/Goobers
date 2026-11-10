package decisiongate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const sampleDiff = "--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n-old\n+new\n+more\n--- a/b.go\n+++ /dev/null\n-gone\n"

func descriptionGate(t *testing.T, f *fake) *Gate {
	t.Helper()
	g, err := New(f, Config{Thresholds: map[string]Threshold{DescriptionQuestion: DefaultDescriptionThreshold}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestParseDiffStats(t *testing.T) {
	s := ParseDiffStats(sampleDiff)
	if len(s.Files) != 1 || s.Files[0] != "a.go" || s.Additions != 2 || s.Deletions != 2 || s.SizeBucket() != "xs" {
		t.Fatalf("stats = %+v bucket %s", s, s.SizeBucket())
	}
}

func TestEvaluateDescriptionThresholds(t *testing.T) {
	for name, tc := range map[string]struct {
		yes  float64
		want DescriptionVerdict
	}{
		"match":    {0.95, DescriptionMatch},
		"mismatch": {0.05, DescriptionMismatch},
		"unsure":   {0.5, DescriptionUnsure},
	} {
		t.Run(name, func(t *testing.T) {
			got := descriptionGate(t, &fake{yes: tc.yes}).EvaluateDescription(context.Background(), "Fix a", "Fixes a.", sampleDiff)
			if got.Verdict != tc.want || got.SizeBucket != "xs" || got.Err != nil {
				t.Fatalf("got %+v, want %q", got, tc.want)
			}
		})
	}
}

func TestEvaluateDescriptionCodeFactsAndFallback(t *testing.T) {
	f := &fake{yes: 0.99}
	g := descriptionGate(t, f)
	if got := g.EvaluateDescription(context.Background(), "", "body", sampleDiff); got.Verdict != DescriptionUnsure {
		t.Fatalf("empty title = %+v", got)
	}
	if got := g.EvaluateDescription(context.Background(), "t", "body", ""); got.Verdict != DescriptionUnsure || got.SizeBucket != "empty" {
		t.Fatalf("empty diff = %+v", got)
	}
	if f.calls.Load() != 0 {
		t.Fatal("model called for code-decided input")
	}
	got := descriptionGate(t, &fake{err: errors.New("down")}).EvaluateDescription(context.Background(), "t", "b", strings.Repeat("+x\n", 20000))
	if got.Verdict != DescriptionUnsure || got.Err == nil || got.SizeBucket != "l" {
		t.Fatalf("error fallback = %+v", got)
	}
}
