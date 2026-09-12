package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mutationsidecar"
)

// Receipts travel in the durable surrender document. The engine then projects
// them through its normal stage mutation path, with no extra live-only events.
// Malformed or credential-bearing evidence must not be silently discarded or
// altered to permit disposal. An oversized surrender is refused by the plane.
func podMutationReceipts() ([]dispatcher.SurrenderedMutation, error) {
	facts, err := mutationsidecar.ReadHandoff(".")
	if err != nil {
		return nil, err
	}
	registry, scrubber := journal.DefaultScrubber()
	for _, name := range []string{dispatcher.EnvPodToken, "GH_TOKEN", "GITHUB_TOKEN", "GOOBERS_GIT_TOKEN"} {
		if value := os.Getenv(name); value != "" {
			registry.Register([]byte(value))
		}
	}
	mutations := make([]dispatcher.SurrenderedMutation, 0, len(facts))
	for _, fact := range facts {
		encoded, err := json.Marshal(fact)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(encoded, scrubber.Scrub(encoded)) {
			return nil, fmt.Errorf("mutation receipt requires credential reconciliation")
		}
		// A structural conversion makes adding a receipt field without
		// updating the surrender wire shape a compile-time error.
		mutations = append(mutations, dispatcher.SurrenderedMutation(fact))
	}
	return mutations, nil
}
