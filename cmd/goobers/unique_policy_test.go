package main

import (
	"slices"
	"testing"
)

func TestSortedUniqueStringPolicies(t *testing.T) {
	if got, want := uniqueSortedLabels([]string{"beta", "", "alpha", "beta"}), []string{"alpha", "beta"}; !slices.Equal(got, want) {
		t.Fatalf("uniqueSortedLabels() = %#v, want %#v", got, want)
	}
	if got, want := distinctSortedStrings([]string{"beta", "", "alpha", "beta"}), []string{"alpha", "beta"}; !slices.Equal(got, want) {
		t.Fatalf("distinctSortedStrings() = %#v, want %#v", got, want)
	}

	for _, command := range diagnosticsContract().StageCommands {
		if !slices.IsSorted(command.Capabilities) {
			t.Fatalf("%s capabilities are not sorted: %#v", command.Command, command.Capabilities)
		}
		if len(slices.Compact(append([]string(nil), command.Capabilities...))) != len(command.Capabilities) {
			t.Fatalf("%s capabilities contain duplicates: %#v", command.Command, command.Capabilities)
		}
	}
}
