package workflowcluster

import (
	"math"
	"sort"
)

// minDistance keeps lambda = 1/distance finite for duplicate points.
const minDistance = 1e-12

type mstEdge struct {
	a, b   int
	weight float64
}

type condensedCluster struct {
	parent    int
	birth     float64
	points    []int
	children  []int
	stability float64
}

// hdbscan labels each row of the symmetric distance matrix d with a cluster
// index or -1 for noise, using mutual-reachability single linkage, a condensed
// tree, and excess-of-mass selection (Campello et al., 2013). It is O(n^2) in
// time and memory, which is acceptable for per-gaggle workflow populations.
// Ties are broken by index so the result is a pure function of d.
func hdbscan(d [][]float64, minClusterSize, minSamples int, allowSingle bool) []int {
	n := len(d)
	labels := make([]int, n)
	for i := range labels {
		labels[i] = -1
	}
	if n < 2 || n < minClusterSize {
		return labels
	}

	children, dist, size := singleLinkage(d, coreDistances(d, min(minSamples, n)))
	clusters := condense(n, children, dist, size, minClusterSize)
	selected := selectClusters(clusters, allowSingle)

	next := 0
	for c, keep := range selected {
		if !keep {
			continue
		}
		for _, p := range clusters[c].points {
			labels[p] = next
		}
		next++
	}
	return labels
}

// coreDistances returns each point's distance to its k-th nearest neighbour,
// counting the point itself as the first.
func coreDistances(d [][]float64, k int) []float64 {
	core := make([]float64, len(d))
	for i, row := range d {
		sorted := append([]float64(nil), row...)
		sort.Float64s(sorted)
		core[i] = sorted[k-1]
	}
	return core
}

// singleLinkage builds the dendrogram of the mutual-reachability minimum
// spanning tree. Leaves are 0..n-1 and merge i is node n+i; the root is 2n-2.
func singleLinkage(d [][]float64, core []float64) (children [][2]int, dist []float64, size []int) {
	n := len(d)
	edges := primMST(d, core)
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].weight != edges[j].weight {
			return edges[i].weight < edges[j].weight
		}
		if edges[i].a != edges[j].a {
			return edges[i].a < edges[j].a
		}
		return edges[i].b < edges[j].b
	})

	children = make([][2]int, 2*n-1)
	dist = make([]float64, 2*n-1)
	size = make([]int, 2*n-1)
	parent := make([]int, n)
	nodeOf := make([]int, n)
	for i := range n {
		parent[i], nodeOf[i], size[i] = i, i, 1
	}
	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	for i, e := range edges {
		ra, rb := find(e.a), find(e.b)
		node := n + i
		children[node] = [2]int{nodeOf[ra], nodeOf[rb]}
		dist[node] = e.weight
		size[node] = size[nodeOf[ra]] + size[nodeOf[rb]]
		parent[rb] = ra
		nodeOf[ra] = node
	}
	return children, dist, size
}

func primMST(d [][]float64, core []float64) []mstEdge {
	n := len(d)
	inTree := make([]bool, n)
	best := make([]float64, n)
	from := make([]int, n)
	for i := range best {
		best[i] = math.Inf(1)
	}
	edges := make([]mstEdge, 0, n-1)
	current := 0
	inTree[0] = true
	for len(edges) < n-1 {
		next := -1
		for j := range n {
			if inTree[j] {
				continue
			}
			if w := max(core[current], core[j], d[current][j]); w < best[j] {
				best[j], from[j] = w, current
			}
			if next == -1 || best[j] < best[next] {
				next = j
			}
		}
		a, b := from[next], next
		if a > b {
			a, b = b, a
		}
		edges = append(edges, mstEdge{a: a, b: b, weight: best[next]})
		inTree[next] = true
		current = next
	}
	return edges
}

// condense walks the dendrogram from the root, keeping a split only when both
// sides have at least minClusterSize points; smaller sides fall out of their
// parent as noise at that density. Cluster 0 is the root, and children always
// have larger indices than their parent.
func condense(n int, children [][2]int, dist []float64, size []int, minClusterSize int) []condensedCluster {
	root := 2*n - 2
	clusters := []condensedCluster{{parent: -1, points: leaves(root, n, children)}}

	fallOut := func(node, c int, lambda float64) {
		clusters[c].stability += float64(size[node]) * (lambda - clusters[c].birth)
	}
	var walk func(node, c int)
	walk = func(node, c int) {
		if node < n {
			return
		}
		left, right := children[node][0], children[node][1]
		lambda := 1 / max(dist[node], minDistance)
		switch bigLeft, bigRight := size[left] >= minClusterSize, size[right] >= minClusterSize; {
		case bigLeft && bigRight:
			for _, child := range []int{left, right} {
				fallOut(child, c, lambda)
				id := len(clusters)
				clusters = append(clusters, condensedCluster{parent: c, birth: lambda, points: leaves(child, n, children)})
				clusters[c].children = append(clusters[c].children, id)
				walk(child, id)
			}
		case bigLeft:
			fallOut(right, c, lambda)
			walk(left, c)
		case bigRight:
			fallOut(left, c, lambda)
			walk(right, c)
		default:
			fallOut(left, c, lambda)
			fallOut(right, c, lambda)
		}
	}
	walk(root, 0)
	return clusters
}

func leaves(node, n int, children [][2]int) []int {
	if node < n {
		return []int{node}
	}
	out := append(leaves(children[node][0], n, children), leaves(children[node][1], n, children)...)
	sort.Ints(out)
	return out
}

// selectClusters applies excess-of-mass selection bottom-up. The root is only
// eligible when allowSingle is set, matching the reference implementation.
func selectClusters(clusters []condensedCluster, allowSingle bool) []bool {
	selected := make([]bool, len(clusters))
	var deselect func(c int)
	deselect = func(c int) {
		for _, child := range clusters[c].children {
			selected[child] = false
			deselect(child)
		}
	}
	for c := len(clusters) - 1; c >= 0; c-- {
		if c == 0 && !allowSingle {
			continue
		}
		if len(clusters[c].children) == 0 {
			selected[c] = true
			continue
		}
		var childSum float64
		for _, child := range clusters[c].children {
			childSum += clusters[child].stability
		}
		if clusters[c].stability >= childSum {
			selected[c] = true
			deselect(c)
		} else {
			clusters[c].stability = childSum
		}
	}
	return selected
}
