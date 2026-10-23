package engine

// Parity row E1-backlog-query-claim-partition — CLOSED by plan item E1
// (#3873); must stay GREEN.
//
// This is the CONSEQUENCE half of rowBacklogQueryDefaults (#3873). That row
// compares the two envelopes and says "the engine is missing an input". This
// one takes the inputs the engine actually handed the stage, compiles them
// with the SAME predicate `goobers backlog-query` compiles from them
// (labelpredicate.Compile over requireLabels/excludeLabels —
// cmd/goobers/backlogquery.go:310-317), and asks the question the operator
// cares about: could this run have claimed the SIBLING instance's item?
//
// The claim partition (MIRC-2) is a pair of instances — the cloud
// (Goobernetes) instance and the laptop instance — sharing one backlog repo
// and splitting it by label: the cloud gaggle declares
// `requireLabels: [goobers:cloud]`, the laptop's declares `goobers:local`.
// Nothing else separates them. The runner injects the gaggle's RequireLabels
// into every `goobers backlog-query` stage that does not declare its own
// (internal/runner/run.go:4413-4414), and that injection IS the partition. An
// engine-driven backlog-curation run with no counterpart hands the stage a
// predicate that matches EVERY approved item in the repo, including the ones
// labelled `goobers:local` that the laptop instance owns — it does not fail,
// it quietly claims the sibling's work.
//
// Why this row exists next to rowBacklogQueryDefaults rather than inside it:
// the sibling-claim reachability is the far-side acceptance criterion of
// decision 005's own evidence list ("a goobers:local item in the same repo is
// never labelled goobers:claimed by app/goobersbot"), and an input-equality
// assertion is not that. A future port that stamped SOME requireLabels value
// onto the stage would satisfy an equality row it was written against; only
// evaluating the predicate against the sibling's item says whether the
// partition holds.
//
// Closed by plan item E1 together with rowBacklogQueryDefaults: the engine
// pins RunInput.BacklogQueryAssignedTo/RequireLabels at start and runTask
// applies internal/backlogdefaults.Apply. Ablating either — the pin in
// Registry.StartInputVersion or the Apply call in runTask — turns this row red
// with "would CLAIM the sibling instance's item".

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/labelpredicate"
)

// The two instances' items as they exist in the shared backlog repo. Both are
// trust-approved and otherwise identical: the partition label is the ONLY
// thing that may keep the cloud instance off the laptop's item.
var (
	parityCloudItemLabels   = []string{"goobers", "goobers:approved", "goobers:cloud"}
	paritySiblingItemLabels = []string{"goobers", "goobers:approved", "goobers:local"}
)

func init() {
	registerParityRow(parityCase{
		Row:                       rowBacklogQueryClaimPartition,
		Name:                      "a cloud run cannot claim the sibling instance's local item",
		Lane:                      "backlog-curation.yaml",
		Build:                     buildBacklogQueryPartitionCase,
		BacklogQueryRequireLabels: parityRequireLabels,
		BacklogQueryAssignedTo:    parityAssignedTo,
		Premise:                   premiseBacklogQueryPartition,
		Check:                     checkBacklogQueryPartition,
	})
}

func buildBacklogQueryPartitionCase(t *testing.T, c *parityCase) {
	t.Helper()
	lane := backlogCurationLane(t)
	// The real production stage, not a contrived one: backlog-curation's
	// query-backlog declares excludeLabels and trustLabel but no
	// requireLabels, so the gaggle default is the only partition it has.
	c.Spec = laneChain(t, lane, "query-backlog")
	c.DSLVersion = lane.DSLVersion
	c.UsesRepo = true
	c.Script = map[string][]scriptedCall{
		"query-backlog": {succeed(map[string]interface{}{"claimed-items": "1"})},
	}
}

// premiseBacklogQueryPartition is the ungraded anti-vacuity half: the RUNNER's
// envelope really does carry a partition that excludes the sibling's item. If
// the runner ever loses the defaulting, or the lane starts declaring its own
// requireLabels, this row stops meaning anything and must fail outright rather
// than be absorbed by parityExpectedFailures.
func premiseBacklogQueryPartition(obs parityObservation) error {
	if err := requireClaimPartition(obs.Runner); err != nil {
		return errParityPremisef(obs.Case.Row, "%v", err)
	}
	return nil
}

// checkBacklogQueryPartition is the gradeable divergence half: the same
// question asked of the engine's envelope.
func checkBacklogQueryPartition(obs parityObservation) error {
	if err := requireClaimPartition(obs.Engine); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	return checkAllSurfaces(obs)
}

// requireClaimPartition compiles the label filter `goobers backlog-query`
// would compile from the envelope this side handed the stage, and asserts it
// selects this instance's item and REJECTS the sibling instance's.
//
// It reads the values off the envelope rather than taking them as parameters
// on purpose: an assertion built from what the row expects, rather than from
// what the stage was handed, cannot see an empty requireLabels at all.
func requireClaimPartition(side paritySide) error {
	dispatches := 0
	for _, env := range side.Envelopes {
		if env.Stage != "query-backlog" {
			continue
		}
		dispatches++
		require, err := parityEnvelopeInputValue(env, "requireLabels")
		if err != nil {
			return err
		}
		exclude, err := parityEnvelopeInputValue(env, "excludeLabels")
		if err != nil {
			return err
		}
		filter, err := labelpredicate.Compile("", splitParityLabelList(require), splitParityLabelList(exclude))
		if err != nil {
			return fmt.Errorf("%s envelope for stage %q (dispatch %d) carries a requireLabels/excludeLabels pair backlog-query cannot compile: %w",
				side.Name, env.Stage, dispatches, err)
		}
		sibling, err := filter.Matches(paritySiblingItemLabels)
		if err != nil {
			return fmt.Errorf("%s envelope for stage %q (dispatch %d): evaluate sibling item: %w", side.Name, env.Stage, dispatches, err)
		}
		if sibling {
			return fmt.Errorf("%s envelope for stage %q (dispatch %d) would CLAIM the sibling instance's item %v: "+
				"the stage was handed requireLabels=%q, which selects the whole backlog instead of the %q partition. inputs were: %s",
				side.Name, env.Stage, dispatches, paritySiblingItemLabels, require, parityRequireLabels, env.Inputs)
		}
		own, err := filter.Matches(parityCloudItemLabels)
		if err != nil {
			return fmt.Errorf("%s envelope for stage %q (dispatch %d): evaluate own item: %w", side.Name, env.Stage, dispatches, err)
		}
		if !own {
			return fmt.Errorf("%s envelope for stage %q (dispatch %d) would claim NOTHING: requireLabels=%q excludes this instance's own item %v. inputs were: %s",
				side.Name, env.Stage, dispatches, require, parityCloudItemLabels, env.Inputs)
		}
		// The claim's assignee is the other half of the partition: an item
		// claimed under the sibling's identity is the sibling's, whatever its
		// labels say.
		assignedTo, err := parityEnvelopeInputValue(env, "assignedTo")
		if err != nil {
			return err
		}
		if assignedTo != parityAssignedTo {
			return fmt.Errorf("%s envelope for stage %q (dispatch %d) claims as %q, not this instance's identity %q. inputs were: %s",
				side.Name, env.Stage, dispatches, assignedTo, parityAssignedTo, env.Inputs)
		}
	}
	if dispatches == 0 {
		if side.Err != nil {
			return fmt.Errorf("%s never dispatched stage %q; its walk ended with: %w", side.Name, "query-backlog", side.Err)
		}
		return fmt.Errorf("%s never dispatched stage %q", side.Name, "query-backlog")
	}
	return nil
}

// parityEnvelopeInputValue reads one resolved input back out of a projected
// envelope, returning "" for an input the stage was never handed — which is
// exactly the state this row exists to catch, so it is a value rather than an
// error.
//
// It decodes the projection encodeParityInputs produced ("key=<json> " joined,
// key-sorted) with a JSON decoder rather than by splitting on spaces, so a
// value containing a space round-trips.
func parityEnvelopeInputValue(env parityEnvelope, key string) (string, error) {
	for _, start := range parityInputKeyOffsets(env.Inputs, key) {
		var value interface{}
		dec := json.NewDecoder(strings.NewReader(env.Inputs[start:]))
		if err := dec.Decode(&value); err != nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return fmt.Sprintf("%v", value), nil
		}
		return text, nil
	}
	return "", nil
}

// parityInputKeyOffsets returns every offset in the encoded input string where
// key's JSON value begins. A key is only a key at the start of the string or
// after a space, so an input named "labels" cannot be found inside
// "requireLabels".
func parityInputKeyOffsets(encoded, key string) []int {
	var offsets []int
	needle := key + "="
	for at := 0; at < len(encoded); {
		i := strings.Index(encoded[at:], needle)
		if i < 0 {
			return offsets
		}
		i += at
		if i == 0 || encoded[i-1] == ' ' {
			offsets = append(offsets, i+len(needle))
		}
		at = i + len(needle)
	}
	return offsets
}

// splitParityLabelList mirrors cmd/goobers' splitLabelList (prselect.go:838),
// the function backlog-query itself parses these inputs with.
func splitParityLabelList(value string) []string {
	var labels []string
	for _, label := range strings.Split(value, ",") {
		if label = strings.TrimSpace(label); label != "" {
			labels = append(labels, label)
		}
	}
	return labels
}
