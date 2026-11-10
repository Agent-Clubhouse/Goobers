package decisioneval

import (
	"context"
	"os"
	"strings"
	"testing"
)

func runRankFixture(t *testing.T, keepReport int) RankReport {
	t.Helper()
	f, err := LoadRankFixture("testdata/knowledge_rank.json")
	if err != nil {
		t.Fatal(err)
	}
	if keepReport > 0 {
		var kept []RankTask
		n := 0
		for _, tk := range f.Tasks {
			if tk.Split == SplitReport {
				if n++; n > keepReport {
					continue
				}
			}
			kept = append(kept, tk)
		}
		f.Tasks = kept
	}
	r, err := RunRank(context.Background(), f, NewReplayScoreDecider(f), RankOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRankReplayBeatsExistingOrder(t *testing.T) {
	r := runRankFixture(t, 0)
	if r.Tasks != 24 || r.Tune != 8 || r.Needed != 48 || r.Thresholds.DropAtOrBelow != 0.95 {
		t.Fatalf("shape wrong: %+v", r)
	}
	if r.Model.P[1].Hits <= r.Existing.P[1].Hits {
		t.Fatalf("model should lift precision@3: model=%+v existing=%+v", r.Model.P[1], r.Existing.P[1])
	}
	if r.Model.FalseDrops != 0 {
		t.Fatalf("needed items dropped: %d", r.Model.FalseDrops)
	}
	if ok, why := r.Graduation(); !ok {
		t.Fatalf("expected ready, got %s", why)
	}
}

func TestRankSmallSampleRefusesPercentages(t *testing.T) {
	r := runRankFixture(t, 5)
	out := r.Render()
	if !strings.Contains(out, "precision@3=n/a (tasks=5<20)") || strings.Contains(out, "%") {
		t.Fatalf("percentages printed for small sample:\n%s", out)
	}
	if ok, _ := r.Graduation(); ok {
		t.Fatal("must not graduate on a small sample")
	}
}

func TestRankFixtureValidation(t *testing.T) {
	f, err := LoadRankFixture("testdata/knowledge_rank.json")
	if err != nil {
		t.Fatal(err)
	}
	f.Provenance.Model = ""
	if f.Validate() == nil {
		t.Fatal("missing provenance must be rejected")
	}
	f.Provenance.Model = "m"
	f.Tasks[0].Candidates[0].Label = 3
	if f.Validate() == nil {
		t.Fatal("out-of-range label must be rejected")
	}
}

func TestRankReportGolden(t *testing.T) {
	got := runRankFixture(t, 0).Render()
	const path = "testdata/rank_report.golden"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatalf("report differs from golden:\n%s", got)
	}
}
