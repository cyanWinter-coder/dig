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

// FindCycles reports every cycle in the graph, one representative cycle per
// strongly connected component that contains a cycle.
//
// Unlike IsAcyclic, which stops at the first cycle it encounters, FindCycles
// reports all independent cycles so that callers can describe every
// conflicting node and edge in a single error.
//
// The result is deterministic: nodes are visited in ascending order, duplicate
// edges are dropped, and each returned cycle is rotated to start at its
// smallest node. A cycle repeats its first node at the end, for example,
// []int{1, 2, 3, 1}.
//
// A strongly connected component that contains a cycle but for which no
// simple cycle can be found yields an empty entry. This cannot happen for
// well-formed graphs, but callers should not assume every entry is non-empty.
func FindCycles(g Graph) [][]int {
	order := g.Order()
	if order <= 0 {
		return nil
	}

	// Freeze the adjacency lists, dropping out-of-range and duplicate
	// edges, and sorting the rest so that traversal does not depend on the
	// order in which edges were reported.
	edges := make([][]int, order)
	for u := 0; u < order; u++ {
		seen := make(map[int]struct{})
		var neighbors []int
		for _, v := range g.EdgesFrom(u) {
			if v < 0 || v >= order {
				continue
			}
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			neighbors = append(neighbors, v)
		}
		sort.Ints(neighbors)
		edges[u] = neighbors
	}

	// Tarjan's algorithm computes the strongly connected components of the
	// graph. A component contains a cycle if it has more than one node or a
	// node with an edge to itself.
	var (
		index    = make([]int, order)
		lowlink  = make([]int, order)
		onStack  = make([]bool, order)
		stack    []int
		next     int
		comps    [][]int
		strongCC func(v int)
	)
	for i := range index {
		index[i] = -1
	}

	strongCC = func(v int) {
		index[v] = next
		lowlink[v] = next
		next++
		stack = append(stack, v)
		onStack[v] = true

		for _, w := range edges[v] {
			if index[w] == -1 {
				strongCC(w)
				if lowlink[w] < lowlink[v] {
					lowlink[v] = lowlink[w]
				}
			} else if onStack[w] && index[w] < lowlink[v] {
				lowlink[v] = index[w]
			}
		}

		if lowlink[v] != index[v] {
			return
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

	for v := 0; v < order; v++ {
		if index[v] == -1 {
			strongCC(v)
		}
	}

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
		if len(comp) == 1 && !contains(edges[start], start) {
			continue
		}

		inComp := make(map[int]struct{}, len(comp))
		for _, v := range comp {
			inComp[v] = struct{}{}
		}
		if cycle := findCycleFrom(edges, inComp, start); len(cycle) > 0 {
			cycles = append(cycles, cycle)
		} else {
			cycles = append(cycles, nil)
		}
	}
	return cycles
}

// findCycleFrom finds a simple cycle that starts and ends at start, using
// only the nodes in comp.
func findCycleFrom(edges [][]int, comp map[int]struct{}, start int) []int {
	path := []int{start}
	visited := map[int]bool{start: true}

	var walk func(u int) bool
	walk = func(u int) bool {
		for _, v := range edges[u] {
			if _, ok := comp[v]; !ok {
				continue
			}
			if v == start {
				path = append(path, start)
				return true
			}
			if visited[v] {
				continue
			}
			visited[v] = true
			path = append(path, v)
			if walk(v) {
				return true
			}
			path = path[:len(path)-1]
			delete(visited, v)
		}
		return false
	}

	if walk(start) {
		return path
	}
	return nil
}

func contains(xs []int, x int) bool {
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
