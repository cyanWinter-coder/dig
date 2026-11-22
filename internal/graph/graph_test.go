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

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

type TestGraph struct {
	Nodes map[int][]int
}

func newTestGraph() *TestGraph {
	return &TestGraph{
		Nodes: make(map[int][]int),
	}
}

func (g TestGraph) Order() int {
	return len(g.Nodes)
}

func (g TestGraph) EdgesFrom(u int) []int {
	return g.Nodes[u]
}

func TestGraphIsAcyclic(t *testing.T) {
	testCases := []struct {
		edges [][]int
	}{
		// 0
		{
			// Edges is an adjacency list representation of
			// a directed graph. i.e. edges[u] is a list of
			// nodes that node u has edges pointing to.
			edges: [][]int{
				nil,
			},
		},
		// 0 --> 1 --> 2
		{
			edges: [][]int{
				{1},
				{2},
				nil,
			},
		},
		// 0 ---> 1 -------> 2
		// |                 ^
		// '-----------------'
		{
			edges: [][]int{
				{1, 2},
				{2},
				nil,
			},
		},
		// 0 --> 1 --> 2    4 --> 5
		// |           ^    ^
		// +-----------'    |
		// '---------> 3 ---'
		{
			edges: [][]int{
				{1, 2, 3},
				{2},
				nil,
				{4},
				{5},
				nil,
			},
		},
	}
	for _, tt := range testCases {
		g := newTestGraph()
		for i, neighbors := range tt.edges {
			g.Nodes[i] = neighbors
		}
		ok, cycle := IsAcyclic(g)
		assert.True(t, ok, "expected acyclic, got cycle %v", cycle)
	}
}

func TestGraphIsCyclic(t *testing.T) {
	testCases := []struct {
		edges [][]int
		cycle []int
	}{
		//
		// 0 ---> 1 ---> 2 ---> 3
		// ^                    |
		// '--------------------'
		{
			edges: [][]int{
				{1},
				{2},
				{3},
				{0},
			},
			cycle: []int{0, 1, 2, 3, 0},
		},
		//
		// 0 ---> 1 ---> 2
		//        ^      |
		//        '------'
		{
			edges: [][]int{
				{1},
				{2},
				{1},
			},
			cycle: []int{1, 2, 1},
		},
		//
		// 0 ---> 1 ---> 2 ----> 3
		// |      ^      |       ^
		// |      '------'       |
		// '---------------------'
		{
			edges: [][]int{
				{1, 3},
				{2},
				{1, 3},
				nil,
			},
			cycle: []int{1, 2, 1},
		},
	}
	for _, tt := range testCases {
		g := newTestGraph()
		for i, neighbors := range tt.edges {
			g.Nodes[i] = neighbors
		}
		ok, c := IsAcyclic(g)
		assert.False(t, ok)
		assert.Equal(t, tt.cycle, c)
	}
}

func TestFindCycles(t *testing.T) {
	testCases := []struct {
		desc  string
		edges [][]int

		// wantCycles lists the expected cycles in order. Each cycle is
		// expected to start and end with its smallest node.
		wantCycles [][]int
	}{
		{
			desc:  "empty graph",
			edges: nil,
		},
		{
			desc: "acyclic graph",
			edges: [][]int{
				{1, 2},
				{2},
				nil,
			},
		},
		{
			desc: "self cycle",
			edges: [][]int{
				{0},
			},
			wantCycles: [][]int{{0, 0}},
		},
		{
			desc: "single cycle",
			edges: [][]int{
				{1},
				{2},
				{3},
				{0},
			},
			wantCycles: [][]int{{0, 1, 2, 3, 0}},
		},
		{
			desc: "cycle reported after unreachable acyclic nodes",
			edges: [][]int{
				{1},
				nil,
				{3},
				{2},
			},
			wantCycles: [][]int{{2, 3, 2}},
		},
		{
			desc: "multiple independent cycles",
			edges: [][]int{
				{1},
				{0},
				{3},
				{2},
			},
			wantCycles: [][]int{
				{0, 1, 0},
				{2, 3, 2},
			},
		},
		{
			desc: "overlapping cycle reports one representative per component",
			edges: [][]int{
				{1, 2},
				{2},
				{1},
			},
			wantCycles: [][]int{{1, 2, 1}},
		},
		{
			// Duplicate edges must not be reported as cycles or cause
			// duplicate path entries.
			desc: "duplicate edges are deduplicated",
			edges: [][]int{
				{1, 1},
				{0, 0},
			},
			wantCycles: [][]int{{0, 1, 0}},
		},
		{
			// The reported cycle must start at the lowest node in the
			// component regardless of the order edges were declared in.
			desc: "cycle starts at smallest node",
			edges: [][]int{
				nil,
				{2},
				{1},
			},
			wantCycles: [][]int{{1, 2, 1}},
		},
		{
			desc: "out of range edges are ignored",
			edges: [][]int{
				{5},
			},
		},
	}

	for _, tt := range testCases {
		t.Run(tt.desc, func(t *testing.T) {
			g := newTestGraph()
			for i, neighbors := range tt.edges {
				g.Nodes[i] = neighbors
			}
			assert.Equal(t, tt.wantCycles, FindCycles(g))
		})
	}
}
