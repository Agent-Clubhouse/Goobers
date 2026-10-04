package decomposition

import (
	"context"
	"testing"

	"github.com/goobers/goobers/providers"
)

// Fixtures for #5255: a plan must distinguish independent reviewability from
// predecessor code availability. The STRUCTURAL cases below are what
// ValidatePlan and the publisher enforce. The SEMANTIC cases pin what they do
// not: a missing dependency, or a closed predecessor that never landed code,
// is invisible to structure and is owned by the decomposer's planning
// guidance. None of these tests treats prose as proof of independence.

const (
	shapeOrdered  = "dependency-ordered"
	shapeCombined = "combined"
	shapeOmitted  = "dependency-omitted"
)

// producerConsumerChildren is a generic producer/consumer contract split plus
// one unrelated child, in the requested shape.
func producerConsumerChildren(shape string) []ChildPlan {
	producer := ChildPlan{
		Key:                "contract",
		Title:              "Add the widget export contract",
		Body:               "Introduce the exported WidgetRecord type and its encoder.",
		Labels:             []string{"area:api", "type:feature"},
		AcceptanceCriteria: "WidgetRecord round-trips through its encoder.",
		ValidationBoundary: "unit tests over the encoder package",
	}
	consumer := ChildPlan{
		Key:                "exporter",
		Title:              "Export widgets through the contract",
		Body:               "Write widgets out using WidgetRecord from the contract child.",
		Labels:             []string{"area:cli", "type:feature"},
		AcceptanceCriteria: "The export command writes WidgetRecord output.",
		ValidationBoundary: "CLI tests over the export command",
	}
	unrelated := ChildPlan{
		Key:                "docs-typos",
		Title:              "Fix typos in the operator guide",
		Body:               "Correct spelling in the operator guide without behavior change.",
		Labels:             []string{"area:docs", "type:chore"},
		AcceptanceCriteria: "The operator guide has no known typos.",
		ValidationBoundary: "markdown link and lint checks",
	}
	switch shape {
	case shapeOrdered:
		consumer.DependsOn = []string{producer.Key}
		return []ChildPlan{producer, consumer, unrelated}
	case shapeCombined:
		combined := producer
		combined.Key = "contract-and-exporter"
		combined.Title = "Add the widget export contract and its exporter"
		combined.Body = "Introduce WidgetRecord and write widgets out through it in one change."
		combined.AcceptanceCriteria = "The export command writes WidgetRecord output that round-trips."
		return []ChildPlan{combined, unrelated}
	default: // shapeOmitted: the consumer names its predecessor only in prose.
		return []ChildPlan{producer, consumer, unrelated}
	}
}

func TestProducerConsumerPlansAreStructurallyValid(t *testing.T) {
	for _, shape := range []string{shapeOrdered, shapeCombined, shapeOmitted} {
		t.Run(shape, func(t *testing.T) {
			live := validLiveParent()
			selection := validSelection(t, live)
			plan := validPlan(selection)
			plan.Children = producerConsumerChildren(shape)
			result := ValidatePlan(plan, selection, live)
			// dependency-omitted passing is deliberate: structural validation
			// cannot prove semantic independence, so it must not reject (or
			// approve) a plan on what its prose says.
			if !result.Valid {
				t.Fatalf("errors = %v, want a structurally valid %s plan", result.Errors, shape)
			}
		})
	}
}

func publishShape(t *testing.T, fake *publisherFake, children []ChildPlan) map[string]providers.WorkItem {
	t.Helper()
	plan := testPublisherPlan()
	plan.Children = children
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	batch, err := (Publisher{
		Provider: fake, Leaser: FileTargetLeaser{Directory: t.TempDir()}, Repo: repo, RunID: "run-5255",
	}).Publish(context.Background(), plan)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	byKey := make(map[string]providers.WorkItem, len(children))
	for i, child := range children {
		byKey[child.Key] = batch.Children[i]
	}
	return byKey
}

// TestPublishedDependenciesGateOnlyRealSuccessors: only dependsOn becomes a
// native blocker. The ordered consumer gets its open producer as a blocker;
// the producer and the unrelated child get none (parallel eligibility); and a
// consumer that names its predecessor only in prose gets no blocker at all.
// That backlog-query skips a candidate with an open native blocker at claim
// time is covered by TestBacklogQueryNativeBlockedByEligibility (#751) in
// cmd/goobers; this test pins only what the publisher attaches.
func TestPublishedDependenciesGateOnlyRealSuccessors(t *testing.T) {
	tests := []struct {
		shape       string
		wantBlocked map[string]string // child key -> blocker child key
	}{
		{shape: shapeOrdered, wantBlocked: map[string]string{"exporter": "contract"}},
		{shape: shapeCombined, wantBlocked: map[string]string{}},
		{shape: shapeOmitted, wantBlocked: map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.shape, func(t *testing.T) {
			fake := newPublisherFake()
			children := producerConsumerChildren(tt.shape)
			byKey := publishShape(t, fake, children)
			for _, child := range children {
				blockers := fake.blockers[byKey[child.Key].ID]
				want, gated := tt.wantBlocked[child.Key]
				if !gated {
					if len(blockers) != 0 {
						t.Fatalf("%s blockers = %v, want none (parallel eligibility)", child.Key, blockers)
					}
					continue
				}
				if len(blockers) != 1 || blockers[0] != byKey[want].ID {
					t.Fatalf("%s blockers = %v, want [%s]", child.Key, blockers, byKey[want].ID)
				}
				if state := fake.items[blockers[0]].State; state != "open" {
					t.Fatalf("%s blocker state = %q, want open (an open native blocker holds the successor at claim time)", child.Key, state)
				}
			}
		})
	}
}

// TestClosedPredecessorIsNotCodeAvailability: depending on an existing issue
// that closed without implementation publishes a CLOSED blocker, which holds
// nothing back, so the dependency proves no code is on the base branch. The
// guidance-conformant plan instead carries the missing work as a sibling
// producer, whose open blocker does hold the consumer until it lands.
func TestClosedPredecessorIsNotCodeAvailability(t *testing.T) {
	const closedPredecessor = "2019"
	newFake := func() *publisherFake {
		fake := newPublisherFake()
		fake.items[closedPredecessor] = providers.WorkItem{
			ID: closedPredecessor, Revision: "r1", Title: "Earlier contract attempt",
			State: "closed", StateReason: "not_planned",
		}
		return fake
	}

	t.Run("depends on closed existing issue", func(t *testing.T) {
		fake := newFake()
		children := producerConsumerChildren(shapeOrdered)[1:] // consumer + unrelated
		children[0].DependsOn = []string{closedPredecessor}
		byKey := publishShape(t, fake, children)
		blockers := fake.blockers[byKey["exporter"].ID]
		if len(blockers) != 1 || blockers[0] != closedPredecessor {
			t.Fatalf("exporter blockers = %v, want [%s]", blockers, closedPredecessor)
		}
		if blocker := fake.items[closedPredecessor]; blocker.State != "closed" {
			t.Fatalf("predecessor state = %q, want closed: the dependency is already satisfied without any code landing", blocker.State)
		}
	})

	t.Run("plans the missing producer as a sibling", func(t *testing.T) {
		fake := newFake()
		byKey := publishShape(t, fake, producerConsumerChildren(shapeOrdered))
		blockers := fake.blockers[byKey["exporter"].ID]
		if len(blockers) != 1 || fake.items[blockers[0]].State != "open" {
			t.Fatalf("exporter blockers = %v, want one open sibling producer", blockers)
		}
	})
}
