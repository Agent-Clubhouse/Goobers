package main

import (
	"reflect"
	"testing"
)

// TestPredecessorBlockers is #991: a parked PR records only the cluster
// members ordered ahead of it (its predecessors), not the symmetric union of
// every overlapping sibling — so a 3+ cluster drains one landing at a time
// instead of deadlocking.
func TestPredecessorBlockers(t *testing.T) {
	cases := []struct {
		name   string
		thisPR int
		blocks []int
		policy electionPolicyFunc
		want   []int
	}{
		// fifo (lowest lands first): predecessors are the LOWER-numbered members.
		{"fifo: last in a 3-cluster waits on both earlier", 13, []int{11, 12}, electedLander, []int{11, 12}},
		{"fifo: middle waits only on the lowest", 12, []int{11, 13}, electedLander, []int{11}},
		{"fifo: the lander (lowest) has no predecessors", 11, []int{12, 13}, electedLander, nil},
		// newest (highest lands first): predecessors are the HIGHER-numbered members.
		{"newest: lowest waits on both higher", 11, []int{12, 13}, electedNewest, []int{12, 13}},
		{"newest: middle waits only on the highest", 12, []int{11, 13}, electedNewest, []int{13}},
		{"newest: the lander (highest) has no predecessors", 13, []int{11, 12}, electedNewest, nil},
		// self is never its own blocker.
		{"self is excluded", 12, []int{11, 12}, electedLander, []int{11}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := predecessorBlockers(c.thisPR, c.blocks, c.policy)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("predecessorBlockers(#%d, %v) = %v, want %v", c.thisPR, c.blocks, got, c.want)
			}
		})
	}
}

// TestClusterDrainsMonotonically is #991's payoff, modeled at the policy level:
// walk a 3-cluster {11,12,13} under fifo and confirm that at each step exactly
// one member has all predecessors landed (is unblocked) and it is the next
// elected lander — i.e. the cluster provably drains rather than deadlocks.
func TestClusterDrainsMonotonically(t *testing.T) {
	cluster := []int{11, 12, 13}
	// predecessors[m] = members ordered before m (its recorded blockers).
	predecessors := map[int][]int{}
	for _, m := range cluster {
		others := []int{}
		for _, o := range cluster {
			if o != m {
				others = append(others, o)
			}
		}
		predecessors[m] = predecessorBlockers(m, others, electedLander)
	}

	landed := map[int]bool{}
	order := []int{}
	for len(order) < len(cluster) {
		// Find the unblocked member (all predecessors landed) — there must be
		// exactly one at each step for a monotone drain.
		var ready []int
		for _, m := range cluster {
			if landed[m] {
				continue
			}
			allLanded := true
			for _, p := range predecessors[m] {
				if !landed[p] {
					allLanded = false
					break
				}
			}
			if allLanded {
				ready = append(ready, m)
			}
		}
		if len(ready) != 1 {
			t.Fatalf("step %d: %d members unblocked, want exactly 1 (order so far %v) — cluster does not drain monotonically", len(order), len(ready), order)
		}
		landed[ready[0]] = true
		order = append(order, ready[0])
	}
	if want := []int{11, 12, 13}; !reflect.DeepEqual(order, want) {
		t.Fatalf("drain order = %v, want %v (fifo)", order, want)
	}
}
