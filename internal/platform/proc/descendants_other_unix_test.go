//go:build unix && !linux && !darwin

package proc

import "testing"

func TestDescendantsFromPS(t *testing.T) {
	output := []byte(" 10 1\n20 10\ninvalid row\n30 20\n40 1\n")
	got := descendantsFromPS(10, output)
	if len(got) != 2 || got[0] != 20 || got[1] != 30 {
		t.Fatalf("descendantsFromPS() = %v, want [20 30]", got)
	}
}
