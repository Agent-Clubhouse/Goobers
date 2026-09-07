package main

import (
	"fmt"

	"github.com/goobers/goobers/internal/labelpredicate"
)

// Park labels are separate from unconditional exclusions: disabling the park
// filter must not weaken an explicit excludeLabels or labelPredicate policy.
// An absent parkLabels list preserves existing custom workflows unchanged.
func compileBacklogLabelSelection(expression string, required, excluded []string, parkLabels, filterParkLabels string) (*labelpredicate.Predicate, []string, error) {
	switch filterParkLabels {
	case "", "true":
		excluded = append(append([]string(nil), excluded...), splitLabelList(parkLabels)...)
	case "false":
		// Opt-out affects only the dedicated park-label list.
	default:
		return nil, nil, fmt.Errorf("filterParkLabels must be true or false")
	}
	predicate, err := labelpredicate.Compile(expression, required, excluded)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid labelPredicate: %w", err)
	}
	return predicate, excluded, nil
}
