// Copyright (c) 2021 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package graph

import "sort"

// Graph represents a simple interface for representation
// of a directed graph.
// It is assumed that each node in the graph is uniquely
// identified with an incremental positive integer (i.e. 1, 2, 3...).
// A value of 0 for a node represents a sentinel error value.
type Graph interface {
	// Order returns the total number of nodes in the graph
	Order() int

	// EdgesFrom returns a list of integers that each
	// represents a node that has an edge from node u.
	EdgesFrom(u int) []int
}

// IsAcyclic uses depth-first search to find cycles
// in a generic graph represented by Graph interface.
// If a cycle is found, it returns a list of nodes that
// are in the cyclic path, identified by their orders.
func IsAcyclic(g Graph) (bool, []int) {
	// cycleStart is a node that introduces a cycle in
	// the graph. Values in the range [1, g.Order()) mean
	// that there exists a cycle in g.
	info := newCycleInfo(g.Order())

	for i := 0; i < g.Order(); i++ {
		info.Reset()

		cycle := isAcyclic(g, i, info, nil /* cycle path */)
		if len(cycle) > 0 {
			return false, cycle
		}
	}

	return true, nil
}

// FindCycles reports every independent cycle in the graph: one
// representative cycle for each strongly connected component that contains
// one. Unlike IsAcyclic, which stops at the first cycle it finds, this
// returns all of them so that callers can describe the complete set of
// problems in a single diagnostic.
//
// The result is deterministic and independent of the order in which the
// graph reports its nodes or edges: duplicate and out-of-range edges are
// dropped, neighbors are visited in ascending order, and each returned
// cycle starts and ends at its smallest node. For example, a cycle over
// nodes 1, 2, and 3 is reported as []int{1, 2, 3, 1}, and a self-loop is
// reported as []int{0, 0}.
//
// The traversal is bounded by the number of nodes, so cycles are never
// followed indefinitely: each node is entered at most once per search.
func FindCycles(g Graph) [][]int {
	order := g.Order()
	if order <= 0 {
		return nil
	}

	edges := make([][]int, order)
	for u := 0; u < order; u++ {
		seen := make(map[int]struct{})
		for _, v := range g.EdgesFrom(u) {
			if v < 0 || v >= order {
				continue
			}
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			edges[u] = append(edges[u], v)
		}
		sort.Ints(edges[u])
	}

	comps := stronglyConnectedComponents(edges)

	// Sort components by their smallest node so that the reported cycles
	// are independent of the order in which components were discovered.
	for _, comp := range comps {
		sort.Ints(comp)
	}
	sort.Slice(comps, func(i, j int) bool {
		return comps[i][0] < comps[j][0]
	})

	var cycles [][]int
	for _, comp := range comps {
		start := comp[0]
		if len(comp) == 1 && !containsInt(edges[start], start) {
			// A single node without a self-loop is not a cycle.
			continue
		}

		inComp := make(map[int]struct{}, len(comp))
		for _, v := range comp {
			inComp[v] = struct{}{}
		}
		cycles = append(cycles, findCycleFrom(edges, inComp, start))
	}
	return cycles
}

// stronglyConnectedComponents returns the strongly connected components of
// the graph described by edges, using an iterative form of Tarjan's
// algorithm. Each node belongs to exactly one component.
func stronglyConnectedComponents(edges [][]int) [][]int {
	order := len(edges)

	var (
		index   = make([]int, order)
		lowlink = make([]int, order)
		onStack = make([]bool, order)
		stack   []int
		next    int
		comps   [][]int
	)
	for i := range index {
		index[i] = -1
	}

	type frame struct {
		v    int
		next int
	}

	for root := 0; root < order; root++ {
		if index[root] != -1 {
			continue
		}

		callStack := []frame{{v: root}}
		index[root] = next
		lowlink[root] = next
		next++
		stack = append(stack, root)
		onStack[root] = true

		for len(callStack) > 0 {
			f := &callStack[len(callStack)-1]
			v := f.v

			if f.next < len(edges[v]) {
				w := edges[v][f.next]
				f.next++

				if index[w] == -1 {
					index[w] = next
					lowlink[w] = next
					next++
					stack = append(stack, w)
					onStack[w] = true
					callStack = append(callStack, frame{v: w})
					continue
				}
				if onStack[w] && index[w] < lowlink[v] {
					lowlink[v] = index[w]
				}
				continue
			}

			// All edges of v have been explored.
			callStack = callStack[:len(callStack)-1]
			if len(callStack) > 0 {
				parent := callStack[len(callStack)-1].v
				if lowlink[v] < lowlink[parent] {
					lowlink[parent] = lowlink[v]
				}
			}

			if lowlink[v] != index[v] {
				continue
			}

			var comp []int
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			comps = append(comps, comp)
		}
	}

	return comps
}

// findCycleFrom returns a simple cycle that starts and ends at start and
// only uses nodes in comp. Because comp is strongly connected, such a cycle
// always exists unless start has no edges within comp.
//
// The cycle is found with a breadth-first search, so the work is bounded by
// the size of the component and the search always terminates: each node is
// entered at most once, and the search never follows a path further than the
// component allows.
func findCycleFrom(edges [][]int, comp map[int]struct{}, start int) []int {
	prev := make(map[int]int)
	visited := map[int]bool{start: true}
	queue := make([]int, 0, len(comp))

	for _, v := range edges[start] {
		if _, ok := comp[v]; !ok {
			continue
		}
		if v == start {
			return []int{start, start}
		}
		if visited[v] {
			continue
		}
		visited[v] = true
		prev[v] = start
		queue = append(queue, v)
	}

	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]

		for _, v := range edges[u] {
			if _, ok := comp[v]; !ok {
				continue
			}
			if v == start {
				// Reconstruct the path start -> ... -> u and close it.
				path := []int{u}
				for cur := u; cur != start; {
					cur = prev[cur]
					path = append(path, cur)
				}
				// path is u ... start; reverse it.
				for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
					path[i], path[j] = path[j], path[i]
				}
				return append(path, start)
			}
			if visited[v] {
				continue
			}
			visited[v] = true
			prev[v] = u
			queue = append(queue, v)
		}
	}

	return nil
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// isAcyclic traverses the given graph starting from a specific node
// using depth-first search using recursion. If a cycle is detected,
// it returns the node that contains the "last" edge that introduces
// a cycle.
// For example, running isAcyclic starting from 1 on the following
// graph will return 3.
//
//	1 -> 2 -> 3 -> 1
func isAcyclic(g Graph, u int, info cycleInfo, path []int) []int {
	// We've already verified that there are no cycles from this node.
	if info[u].Visited {
		return nil
	}
	info[u].Visited = true
	info[u].OnStack = true

	path = append(path, u)
	for _, v := range g.EdgesFrom(u) {
		if !info[v].Visited {
			if cycle := isAcyclic(g, v, info, path); len(cycle) > 0 {
				return cycle
			}
		} else if info[v].OnStack {
			// We've found a cycle, and we have a full path back.
			// Prune it down to just the cyclic nodes.
			cycle := path
			for i := len(cycle) - 1; i >= 0; i-- {
				if cycle[i] == v {
					cycle = cycle[i:]
					break
				}
			}

			// Complete the cycle by adding this node to it.
			return append(cycle, v)
		}
	}
	info[u].OnStack = false
	return nil
}

// cycleNode keeps track of a single node's info for cycle detection.
type cycleNode struct {
	Visited bool
	OnStack bool
}

// cycleInfo contains information about each node while we're trying to find
// cycles.
type cycleInfo []cycleNode

func newCycleInfo(order int) cycleInfo {
	return make(cycleInfo, order)
}

func (info cycleInfo) Reset() {
	for i := range info {
		info[i].OnStack = false
	}
}
