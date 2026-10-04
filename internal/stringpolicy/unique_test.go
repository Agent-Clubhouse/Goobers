package stringpolicy

import (
	"reflect"
	"testing"
)

func TestUniquePreservesFirstSeenExactValues(t *testing.T) {
	input := []string{"beta", "", "alpha", "beta", " alpha ", ""}
	got := Unique(input)
	want := []string{"beta", "", "alpha", " alpha "}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Unique() = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(input, []string{"beta", "", "alpha", "beta", " alpha ", ""}) {
		t.Fatalf("Unique() mutated input: %#v", input)
	}
}

func TestAppendUnique(t *testing.T) {
	values := []string{"alpha", ""}
	values = AppendUnique(values, "alpha")
	values = AppendUnique(values, "")
	values = AppendUnique(values, "beta")
	if want := []string{"alpha", "", "beta"}; !reflect.DeepEqual(values, want) {
		t.Fatalf("AppendUnique() = %#v, want %#v", values, want)
	}
}
