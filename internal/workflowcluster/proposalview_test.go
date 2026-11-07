package workflowcluster

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestScanChildProposalInsightsReadsRetainedLineage(t *testing.T) {
	root := t.TempDir()
	occurrences := writeTrendParent(t, root, "parent-1", trendDigest("1"), [][2]string{{"plan", "m1"}})
	writeTrendChild(t, root, "child-1", "parent-1", occurrences[0])

	view, err := ScanChildProposalInsights(context.Background(), root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if view.Candidates != 2 || view.Excluded != 0 || len(view.Gaggles) != 1 {
		t.Fatalf("view = %+v", view)
	}
	group := view.Gaggles[0]
	if group.Gaggle != "g" || len(group.Proposals) != 1 {
		t.Fatalf("group = %+v", group)
	}
	proposal := group.Proposals[0]
	if proposal.ID != "child-1" || proposal.ParentRunID != "parent-1" ||
		proposal.StageOccurrence != occurrences[0] || proposal.SourceDigest == "" ||
		proposal.EffectiveVersion == "" || proposal.Outcome != journal.PhaseCompleted {
		t.Fatalf("proposal = %+v", proposal)
	}
}

func TestRunCLIEmitsSharedChildProposalView(t *testing.T) {
	root := t.TempDir()
	occurrences := writeTrendParent(t, root, "parent-1", trendDigest("1"), [][2]string{{"plan", "m1"}})
	writeTrendChild(t, root, "child-1", "parent-1", occurrences[0])

	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"--proposals", "--gaggle", "g", "--json", root},
		flag.NewFlagSet("clusters", flag.ContinueOnError), &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	var view ChildProposalView
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Gaggles) != 1 || view.Gaggles[0].Gaggle != "g" ||
		len(view.Gaggles[0].Proposals) != 1 || view.Gaggles[0].Proposals[0].ID != "child-1" {
		t.Fatalf("view = %+v", view)
	}
}

func TestAnalyzeChildProposalObservationsPartitionsGagglesAndLinksEvidence(t *testing.T) {
	var observations []childProposalObservation
	for _, point := range append(blob("a", 0, 0, 4), blob("b", 10, 0, 4)...) {
		observations = append(observations, childProposalObservation{
			insight: ChildProposalInsight{
				ID: point.ID, Gaggle: "core", SourceDigest: "sha256:" + fmt.Sprintf("%064s", point.ID),
				ParentRunID: "parent-" + point.ID, StageOccurrence: "author/0/visit/1",
				EffectiveVersion: "sha256:effective", Outcome: journal.PhaseCompleted,
			},
			point: point,
		})
	}
	observations = append(observations, childProposalObservation{
		insight: ChildProposalInsight{
			ID: "ops-child", Gaggle: "ops", SourceDigest: "sha256:digest",
			ParentRunID: "ops-parent", StageOccurrence: "author/0/visit/1",
			EffectiveVersion: "sha256:ops", Outcome: journal.PhaseFailed,
			Novelty: &journal.ChildNovelty{Scored: true, ModelID: "model", ModelVersion: "v1", FeatureVersion: "f1", OutOfDistribution: true, Distance: 1},
		},
		point: Point{ID: "ops-child", Numeric: map[string]float64{"x": 1}},
	})

	gaggles, err := analyzeChildProposalObservations(observations, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaggles) != 2 || gaggles[0].Gaggle != "core" || gaggles[1].Gaggle != "ops" {
		t.Fatalf("gaggles = %+v", gaggles)
	}
	core := gaggles[0]
	if core.Status != ViewClustered || core.Model == nil || core.Model.Population != 8 || len(core.Map) != 8 || len(core.Proposals) != 8 {
		t.Fatalf("core = %+v", core)
	}
	for _, proposal := range core.Proposals {
		if proposal.ParentRunID == "" || proposal.StageOccurrence == "" || proposal.EffectiveVersion == "" {
			t.Fatalf("proposal lost drill-down evidence: %+v", proposal)
		}
	}
	ops := gaggles[1]
	if ops.Status != ViewInsufficientData || ops.Model != nil || len(ops.Proposals) != 1 ||
		ops.Proposals[0].Novelty == nil || !ops.Proposals[0].Novelty.OutOfDistribution {
		t.Fatalf("ops = %+v", ops)
	}
}
