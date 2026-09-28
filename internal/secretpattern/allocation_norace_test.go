//go:build !race

package secretpattern

import (
	"bytes"
	"strconv"
	"testing"
)

// Allocation counts describe production builds. The race runtime randomly
// discards sync.Pool entries used by regexp, so its extra engine allocations
// cannot satisfy this deterministic budget. Redaction/ownership differential
// tests remain enabled under -race in allocation_test.go.
func TestScrubOrdinaryPayloadAllocations(t *testing.T) {
	for _, size := range []int{1024, 32 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			s := NewScrubber()
			input := bytes.Repeat([]byte("x"), size)
			var output []byte
			allocations := testing.AllocsPerRun(100, func() { output = s.Scrub(input) })
			if !bytes.Equal(input, output) {
				t.Fatal("ordinary payload changed")
			}
			// The two prefixless authorization patterns retain their original
			// replacement path; the other eight must not copy this payload.
			if allocations > 5 {
				t.Fatalf("ordinary payload allocated %.1f times; want <=5, not a copy per pattern", allocations)
			}
			output[0] = 'y'
			if input[0] != 'x' {
				t.Fatal("output aliases caller's input")
			}
		})
	}
}
