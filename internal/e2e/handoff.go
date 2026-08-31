package e2e

import (
	"fmt"
)

// DeclaredEdgeHandoffObserver is S3's named observer (goobernetes-smoke.md
// §4 S3): "three records — (a) the declared edge in the applied workflow;
// (b) runner.* events showing distinct node names for the two stages;
// (c) local-ci's journal shows it operated on the implement commit."
const DeclaredEdgeHandoffObserver = "declared repoFrom coverage (internal/workflow/v_3_0/repofrom.go RepoFromCoverage) + StageAttempt.Placement.Node for both stages + the pushed/checked-out branch head SHA"

// RepoHandoffEvidence is what S3 needs about one producer→consumer pair.
// Node/SHA fields are supplied by the caller rather than read from a fixed
// journal field, because the RUNTIME half of declared-edge enforcement
// (recording the branch head around every non-producer repo stage) is
// explicitly future work — internal/workflow/v_3_0/repofrom.go's own package
// doc: "Compile-half only (issue #3505 / §9 item 7): the actor-scoped
// runtime enforcement... is the transport wave's work, not the
// interpreter's." goobernetes-smoke.md §4 S3 agrees: "S3 exercises the
// runtime half." Until #3505 lands a concrete journal field for "branch head
// SHA at push"/"SHA a fresh worktree checked out", a live driver reads these
// off `git rev-parse` at the two boundaries directly (or the mode-3
// transport's own recording once #3505 ships) and passes them here —
// exactly the seam this task's scope discipline asks for.
type RepoHandoffEvidence struct {
	Producer, Consumer string
	// ProducerNode/ConsumerNode are each stage's StageAttempt.Placement.Node.
	ProducerNode, ConsumerNode string
	// PushedSHA is the branch head SHA recorded at the producer's push
	// boundary; CheckedOutSHA is the SHA the consumer's fresh worktree
	// checked out. Equal means continuity held.
	PushedSHA, CheckedOutSHA string
}

// AssertDeclaredEdgeHandoff is S3: the implement→local-ci (or any declared
// producer→consumer) chain runs on different nodes, with the edge declared,
// and the consumer's fresh worktree actually saw the producer's commit.
//
// declaredCoverage is exactly what internal/workflow/v_3_0.RepoFromCoverage
// returns for the applied workflow: consumer task name -> its set of
// reaching producer names. evidence is the runtime observation for one
// producer/consumer pair a live run (or this test's fixture) recorded.
func AssertDeclaredEdgeHandoff(declaredCoverage map[string][]string, evidence RepoHandoffEvidence) AssertionResult {
	if declaredCoverage == nil {
		return invalid("no declared repoFrom coverage supplied — the applied workflow's compile-time coverage was never captured", nil)
	}
	if evidence.Producer == "" || evidence.Consumer == "" {
		return invalid("evidence names no producer/consumer pair", nil)
	}

	producers, consumerDeclared := declaredCoverage[evidence.Consumer]
	if !consumerDeclared {
		return classify("", false,
			fmt.Sprintf("consumer stage %q is not a declared repo consumer at all (RepoFromCoverage has no entry for it)", evidence.Consumer),
			nil, declaredCoverage)
	}
	declared := false
	for _, p := range producers {
		if p == evidence.Producer {
			declared = true
			break
		}
	}
	if !declared {
		return classify("", false,
			fmt.Sprintf("edge %s->%s is not declared: consumer %q's coverage is %v", evidence.Producer, evidence.Consumer, evidence.Consumer, producers),
			nil, producers)
	}

	if evidence.ProducerNode == "" || evidence.ConsumerNode == "" {
		return invalid(fmt.Sprintf("missing Placement.Node for producer %q or consumer %q", evidence.Producer, evidence.Consumer), evidence)
	}
	if evidence.ProducerNode == evidence.ConsumerNode {
		return classify("", false,
			fmt.Sprintf("producer %q and consumer %q ran on the SAME node %q — S3 requires distinct nodes", evidence.Producer, evidence.Consumer, evidence.ProducerNode),
			nil, evidence)
	}

	if evidence.PushedSHA == "" || evidence.CheckedOutSHA == "" {
		return invalid(fmt.Sprintf("missing pushed/checked-out SHA for %s->%s — the runtime continuity observer did not record one", evidence.Producer, evidence.Consumer), evidence)
	}
	if evidence.PushedSHA != evidence.CheckedOutSHA {
		return classify("", false,
			fmt.Sprintf("SHA discontinuity: %s pushed %s, %s checked out %s — the silent-worktree-continuity assumption S3 exists to falsify", evidence.Producer, evidence.PushedSHA, evidence.Consumer, evidence.CheckedOutSHA),
			nil, evidence)
	}

	return classify("", true, "", evidence, nil)
}
