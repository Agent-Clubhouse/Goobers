package intsplit

import (
	"math"
	"reflect"
	"testing"
)

func TestLargestRemainder(t *testing.T) {
	tests := []struct {
		name    string
		total   int64
		keys    []string
		weights map[string]int64
		want    map[string]int64
	}{
		{
			name:    "exact division",
			total:   12,
			keys:    []string{"b", "a"},
			weights: map[string]int64{"a": 1, "b": 2},
			want:    map[string]int64{"a": 4, "b": 8},
		},
		{
			name:    "non-zero remainders",
			total:   11,
			keys:    []string{"c", "a", "b"},
			weights: map[string]int64{"a": 1, "b": 2, "c": 3},
			want:    map[string]int64{"a": 2, "b": 4, "c": 5},
		},
		{
			name:    "ties break by key",
			total:   2,
			keys:    []string{"charlie", "bravo", "alpha"},
			weights: map[string]int64{"alpha": 1, "bravo": 1, "charlie": 1},
			want:    map[string]int64{"alpha": 1, "bravo": 1, "charlie": 0},
		},
		{
			name:    "zero total and empty key",
			total:   0,
			keys:    []string{"", "other"},
			weights: map[string]int64{"": 1, "other": 1},
			want:    map[string]int64{"": 0, "other": 0},
		},
		{
			name:    "zero weight",
			total:   5,
			keys:    []string{"zero", "weighted"},
			weights: map[string]int64{"zero": 0, "weighted": 2},
			want:    map[string]int64{"zero": 0, "weighted": 5},
		},
		{
			name:    "empty keys",
			total:   5,
			keys:    nil,
			weights: map[string]int64{},
			want:    map[string]int64{},
		},
		{
			name:    "large product",
			total:   math.MaxInt64,
			keys:    []string{"b", "a"},
			weights: map[string]int64{"a": 2, "b": 1},
			want: map[string]int64{
				"a": 6_148_914_691_236_517_205,
				"b": 3_074_457_345_618_258_602,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalKeys := append([]string(nil), tt.keys...)
			got := LargestRemainder(tt.total, tt.keys, func(key string) int64 {
				return tt.weights[key]
			})
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("LargestRemainder() = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.keys, originalKeys) {
				t.Fatalf("LargestRemainder() mutated keys: got %v, want %v", tt.keys, originalKeys)
			}
		})
	}
}
