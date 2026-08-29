package readmodel

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestRandomizedInterventionRequiresExplicitMarker(t *testing.T) {
	randomized, arm, ok := randomizedInterventionFromOutputs(map[string]any{
		"arm": "treatment",
	})
	if ok || randomized || arm != "" {
		t.Fatalf("arm-only output = randomized:%v arm:%q ok:%v, want observational", randomized, arm, ok)
	}

	randomized, arm, ok = randomizedInterventionFromOutputs(map[string]any{
		"randomizedIntervention": true,
		"arm":                    "treatment",
	})
	if !ok || randomized || arm != "treatment" {
		t.Fatalf("untrusted marker = randomized:%v arm:%q ok:%v, want observational", randomized, arm, ok)
	}

	randomized, arm, ok = randomizedInterventionFromOutputs(map[string]any{
		"randomizedIntervention":       true,
		"randomizedInterventionSource": "bandit-assignment",
		"arm":                          "treatment",
	})
	if !ok || !randomized || arm != "treatment" {
		t.Fatalf("marked assignment = randomized:%v arm:%q ok:%v", randomized, arm, ok)
	}
}

func TestCausalCreditTreatsArmOnlyFactsAsUnidentifiable(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	start := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		seedCausalRun(t, store, fmt.Sprintf("arm-only-%02d", i),
			start.Add(time.Duration(i)*time.Minute), "implement", "sha256:v1", "",
			false, "treatment", i%2 == 0, nil)
	}

	got, err := store.CausalCredit(ctx, CausalOptions{
		Gaggle: "core", Workflow: "implementation", MinCohortSize: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Identification != CausalUnidentifiable {
		t.Fatalf("arm-only causal credit = %+v, want unidentifiable", got)
	}
}

func TestCausalCreditUsesRandomizedArmsFromProjectedJournalFacts(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	start := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		runID := fmt.Sprintf("randomized-control-%02d", i)
		seedCausalRun(t, store, runID, start.Add(time.Duration(i)*time.Minute), "implement", "sha256:v1", "", true, "control", false, nil)
	}
	for i := 0; i < 20; i++ {
		runID := fmt.Sprintf("randomized-treatment-%02d", i)
		seedCausalRun(t, store, runID, start.Add(time.Duration(30+i)*time.Minute), "implement", "sha256:v1", "", true, "treatment", true, nil)
	}

	got, err := store.CausalCredit(ctx, CausalOptions{Gaggle: "core", Workflow: "implementation", MinCohortSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Node != "stage:implement" || got[0].Identification != CausalRandomized {
		t.Fatalf("causal credit = %+v, want randomized stage estimate", got)
	}
}

func TestCausalCreditUsesDeclaredTopologyParentsAsCovariates(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	start := time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)
	parents := []NodeParentRow{{
		Kind: "stage", Name: "implement", ParentKind: "stage", ParentName: "query-backlog",
	}}
	for i := 0; i < 12; i++ {
		seedCausalRun(t, store, fmt.Sprintf("old-%02d", i), start.Add(time.Duration(i)*time.Minute),
			"implement", "sha256:old", "sha256:query-a", false, "", false, withChildIdentity(parents, "sha256:old"))
		seedCausalRun(t, store, fmt.Sprintf("new-%02d", i), start.Add(time.Duration(40+i)*time.Minute),
			"implement", "sha256:new", "sha256:query-b", false, "", true, withChildIdentity(parents, "sha256:new"))
	}
	got, err := store.CausalCredit(ctx, CausalOptions{Gaggle: "core", Workflow: "implementation", MinCohortSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	var implement *CausalNodeCredit
	for i := range got {
		if got[i].Node == "stage:implement" {
			implement = &got[i]
		}
	}
	if implement == nil || implement.Identification != CausalUnidentifiable {
		t.Fatalf("causal credit = %+v, want overlap failure from parent covariate", got)
	}
}

func TestCausalCreditBuildsRoutedAndUnroutedDifferenceInDifferencesCohorts(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	start := time.Date(2026, 8, 22, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		seedCausalRun(t, store, fmt.Sprintf("treated-before-%02d", i), at,
			"implement", "sha256:old", "", false, "", true, nil)
		seedCausalControlRun(t, store, fmt.Sprintf("control-before-%02d", i), at.Add(30*time.Second), true)
	}
	for i := 0; i < 10; i++ {
		at := start.Add(time.Hour + time.Duration(i)*time.Minute)
		seedCausalRun(t, store, fmt.Sprintf("treated-after-%02d", i), at,
			"implement", "sha256:new", "", false, "", false, nil)
		seedCausalControlRun(t, store, fmt.Sprintf("control-after-%02d", i), at.Add(30*time.Second), true)
	}

	got, err := store.CausalCredit(ctx, CausalOptions{
		Gaggle: "core", Workflow: "implementation", MinCohortSize: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Identification != CausalDifferenceInDifferences {
		t.Fatalf("causal credit = %+v, want difference-in-differences", got)
	}
	if got[0].Effect != -1 || got[0].TreatedBefore != 10 || got[0].TreatedAfter != 10 ||
		got[0].ControlBefore != 10 || got[0].ControlAfter != 10 ||
		!got[0].PromotionEligible {
		t.Fatalf("DiD estimate = %+v", got[0])
	}
}

func TestCausalCreditAlwaysRoutedChangepointIsIneligibleFallback(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	start := time.Date(2026, 8, 22, 5, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		seedCausalRun(t, store, fmt.Sprintf("old-%02d", i), start.Add(time.Duration(i)*time.Minute),
			"implement", "sha256:old", "", false, "", true, nil)
		seedCausalRun(t, store, fmt.Sprintf("new-%02d", i), start.Add(time.Hour+time.Duration(i)*time.Minute),
			"implement", "sha256:new", "", false, "", false, nil)
	}

	got, err := store.CausalCredit(ctx, CausalOptions{
		Gaggle: "core", Workflow: "implementation", MinCohortSize: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Identification != CausalUnidentifiable ||
		got[0].Effect != -1 || got[0].IntervalAvailable || got[0].PromotionEligible ||
		got[0].PromotionSource != "correlational-fallback" {
		t.Fatalf("causal credit = %+v, want preserved ineligible fallback", got)
	}
}

func withChildIdentity(parents []NodeParentRow, identity string) []NodeParentRow {
	out := append([]NodeParentRow(nil), parents...)
	for i := range out {
		out[i].Identity = identity
	}
	return out
}

func seedCausalControlRun(t *testing.T, store *Store, runID string, startedAt time.Time, failed bool) {
	t.Helper()
	phase := journal.PhaseCompleted
	verdict, target := "pass", journal.TargetComplete
	if failed {
		phase, verdict, target = journal.PhaseFailed, "fail", "@abort"
	}
	finishedAt := startedAt.Add(time.Minute)
	if err := store.UpsertRun(context.Background(), Projection{Run: RunRow{
		RunID: runID, Gaggle: "core", Workflow: "implementation",
		Phase: phase, Terminal: true, StartedAt: startedAt,
		FinishedAt: &finishedAt, LastActivity: finishedAt, LastSeq: 1,
		OutcomeVerdict: verdict, OutcomeTarget: target,
	}}); err != nil {
		t.Fatalf("seed %s: %v", runID, err)
	}
}

func seedCausalRun(
	t *testing.T,
	store *Store,
	runID string,
	startedAt time.Time,
	stage string,
	identity string,
	parentIdentity string,
	randomized bool,
	arm string,
	failed bool,
	parents []NodeParentRow,
) {
	t.Helper()
	phase := journal.PhaseCompleted
	verdict, target := "pass", journal.TargetComplete
	if failed {
		phase, verdict, target = journal.PhaseFailed, "fail", "@abort"
	}
	finishedAt := startedAt.Add(time.Minute)
	nodes := []NodeRow{
		{RunID: runID, Kind: "stage", Name: stage, Identity: identity, Randomized: randomized, Arm: arm, Attempts: 1},
	}
	if parentIdentity != "" {
		nodes = append(nodes, NodeRow{RunID: runID, Kind: "stage", Name: "query-backlog", Identity: parentIdentity, Attempts: 1})
	}
	for i := range parents {
		parents[i].RunID = runID
	}
	if err := store.UpsertRun(context.Background(), Projection{
		Run: RunRow{
			RunID: runID, Gaggle: "core", Workflow: "implementation",
			Phase: phase, Terminal: true, StartedAt: startedAt,
			FinishedAt: &finishedAt, LastActivity: finishedAt, LastSeq: 1,
			OutcomeVerdict: verdict, OutcomeTarget: target,
		},
		Nodes:       nodes,
		NodeParents: parents,
	}); err != nil {
		t.Fatalf("seed %s: %v", runID, err)
	}
}
