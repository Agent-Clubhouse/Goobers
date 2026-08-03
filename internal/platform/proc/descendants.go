//go:build unix

package proc

func collectDescendants(root int, parents map[int][]int) []int {
	var descendants []int
	queue := append([]int(nil), parents[root]...)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		descendants = append(descendants, pid)
		queue = append(queue, parents[pid]...)
	}
	return descendants
}
