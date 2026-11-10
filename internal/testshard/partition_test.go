package testshard

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
)

func TestPartitionAssignsEveryDistinctNameExactlyOnce(t *testing.T) {
	names := []string{"TestD", "TestA", "TestC", "TestB", "TestA", "TestD"}
	original := slices.Clone(names)
	for _, pieces := range []int{1, 2, 3, 8} {
		t.Run(fmt.Sprint(pieces), func(t *testing.T) {
			groups := Partition(names, pieces, func(string) int64 { return 1 })
			if len(groups) != pieces {
				t.Fatalf("got %d groups, want %d", len(groups), pieces)
			}
			seen := map[string]int{}
			for _, group := range groups {
				if !slices.IsSorted(group) {
					t.Errorf("group is not sorted: %v", group)
				}
				for _, name := range group {
					seen[name]++
				}
			}
			want := map[string]int{"TestA": 1, "TestB": 1, "TestC": 1, "TestD": 1}
			if !reflect.DeepEqual(seen, want) {
				t.Fatalf("assignments = %v, want %v", seen, want)
			}
			if !slices.Equal(names, original) {
				t.Fatalf("input changed from %v to %v", original, names)
			}
		})
	}
	if got := Partition(nil, 3, func(string) int64 { return 1 }); !reflect.DeepEqual(got, make([][]string, 3)) {
		t.Fatalf("empty input = %v, want three empty groups", got)
	}
}

func TestPartitionIsIndependentOfEnumerationAndWeightMapOrder(t *testing.T) {
	entries := []struct {
		name   string
		weight int64
	}{
		{"TestA", 10}, {"TestB", 10}, {"TestC", 6}, {"TestD", 4}, {"TestE", 1},
	}
	// Equal initial weights select A before B; after C and D fill the last
	// group, E must choose group zero from three equal totals.
	want := [][]string{{"TestA", "TestE"}, {"TestB"}, {"TestC", "TestD"}}
	for offset := range len(entries) {
		for _, reverse := range []bool{false, true} {
			weights := make(map[string]int64, len(entries))
			var names []string
			for index := range len(entries) {
				entry := entries[(index+offset)%len(entries)]
				weights[entry.name] = entry.weight
				names = append(names, entry.name)
			}
			if reverse {
				slices.Reverse(names)
			}
			weigh := func(name string) int64 { return weights[name] }
			if got := Partition(names, 3, weigh); !reflect.DeepEqual(got, want) {
				t.Fatalf("offset=%d reverse=%v: groups = %v, want %v", offset, reverse, got, want)
			}
			var mapNames []string
			for name := range weights {
				mapNames = append(mapNames, name)
			}
			if got := Partition(mapNames, 3, weigh); !reflect.DeepEqual(got, want) {
				t.Fatalf("map enumeration %v: groups = %v, want %v", mapNames, got, want)
			}
		}
	}
}

func TestPartitionBalancesKnownHeavyTests(t *testing.T) {
	weights := map[string]int64{
		"TestHeavyA": 100, "TestHeavyB": 90, "TestHeavyC": 80,
		"TestSmallA": 20, "TestSmallB": 10,
	}
	names := []string{"TestSmallB", "TestHeavyB", "TestSmallA", "TestHeavyC", "TestHeavyA"}
	groups := Partition(names, 3, func(name string) int64 { return weights[name] })
	for index, group := range groups {
		var total int64
		for _, name := range group {
			total += weights[name]
		}
		if total != 100 {
			t.Errorf("group %d weighs %d, want 100: %v", index, total, group)
		}
	}
}
