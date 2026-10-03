package providers

import (
	"slices"
	"testing"
)

func TestUniqueStringsTrimsDropsEmptyAndPreservesFirstSeenOrder(t *testing.T) {
	input := []string{" beta ", "", "alpha", "beta", " alpha ", "gamma"}
	got := uniqueStrings(input)
	want := []string{"beta", "alpha", "gamma"}
	if !slices.Equal(got, want) {
		t.Fatalf("uniqueStrings() = %#v, want %#v", got, want)
	}
	if !slices.Equal(input, []string{" beta ", "", "alpha", "beta", " alpha ", "gamma"}) {
		t.Fatalf("uniqueStrings() mutated input: %#v", input)
	}
}
