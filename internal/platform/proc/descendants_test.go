package proc

import (
	"slices"
	"testing"
	"time"
)

// The plain case first, so the guard below cannot pass by walking nothing.
func TestCollectDescendantsWalksTheWholeSubtree(t *testing.T) {
	parents := map[int][]int{
		1:  {10, 40},
		10: {20, 30},
		20: {21},
	}
	got := collectDescendants(10, parents)
	slices.Sort(got)
	if want := []int{20, 21, 30}; !slices.Equal(got, want) {
		t.Fatalf("collectDescendants = %v, want %v", got, want)
	}
}

// #3922: a process table whose recorded parentage loops must not hang the
// walk. This is not a hypothetical shape. A Windows process entry carries the
// pid of whatever created it and keeps carrying it after that creator exits,
// so a recycled pid can name a process further DOWN the same subtree; the
// unix collectors read /proc or ps one row at a time, so rows recorded on
// either side of a reparent can describe the same loop.
//
// Unguarded, the walk enumerates the cycle forever. Tree.kill calls it while
// terminating, so the caller never returns and the test binary dies on its
// package timeout ten minutes later — which is how this arrived, as a
// "panic: test timed out" attributed to whichever test was running.
//
// The bound is what makes this a regression test rather than a demonstration:
// before the visited set it does not fail, it never finishes.
func TestCollectDescendantsTerminatesOnCyclicParentage(t *testing.T) {
	for name, parents := range map[string]map[int][]int{
		"two-pid cycle":       {10: {20}, 20: {30}, 30: {20}},
		"cycle back to root":  {10: {20}, 20: {10}},
		"self-parenting pid":  {10: {20}, 20: {20}},
		"pid reachable twice": {10: {20, 30}, 20: {40}, 30: {40}, 40: {50}},
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan []int, 1)
			go func() { done <- collectDescendants(10, parents) }()
			select {
			case got := <-done:
				if len(got) > len(parents)*4 {
					t.Fatalf("collectDescendants returned %d pids for a %d-entry table; "+
						"the walk is revisiting pids", len(got), len(parents))
				}
				seen := map[int]bool{}
				for _, pid := range got {
					if seen[pid] {
						t.Fatalf("collectDescendants = %v, which names pid %d twice", got, pid)
					}
					seen[pid] = true
				}
				if seen[10] {
					t.Fatalf("collectDescendants = %v, which names the root; a tree walk "+
						"that returns its own root makes a killer terminate itself", got)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("collectDescendants did not terminate on cyclic parentage (#3922): " +
					"Tree.kill calls this while terminating, so the caller wedges until the " +
					"whole package times out")
			}
		})
	}
}
