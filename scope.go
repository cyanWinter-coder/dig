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

package dig

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"time"

	"go.uber.org/dig/internal/digclock"
)

// A ScopeOption modifies the default behavior of Scope; currently,
// there are no implementations.
type ScopeOption interface {
	noScopeOption() // yet
}

// Scope is a scoped DAG of types and their dependencies.
// A Scope may also have one or more child Scopes that inherit
// from it.
type Scope struct {
	// This implements containerStore interface.

	// Name of the Scope
	name string
	// Mapping from key to all the constructor node that can provide a value for that
	// key.
	providers map[key][]*constructorNode

	// Mapping from key to the decorator that decorates a value for that key.
	decorators map[key]*decoratorNode

	// constructorNodes provided directly to this Scope. i.e. it does not include
	// any nodes that were provided to the parent Scope this inherited from.
	nodes []*constructorNode

	// Values that generated via decorators in the Scope.
	decoratedValues map[key]reflect.Value

	// Values that generated directly in the Scope.
	values map[key]reflect.Value

	// Values groups that generated directly in the Scope.
	groups map[key][]reflect.Value

	// Values groups that generated via decoraters in the Scope.
	decoratedGroups map[key]reflect.Value

	// Source of randomness.
	rand *rand.Rand

	// Flag indicating whether the graph has been checked for cycles.
	isVerifiedAcyclic bool

	// Defer acyclic check on provide until Invoke.
	deferAcyclicVerification bool

	// Recover from panics in user-provided code and wrap in an exported error type.
	recoverFromPanics bool

	// invokerFn calls a function with arguments provided to Provide or Invoke.
	invokerFn invokerFn

	// graph of this Scope. Note that this holds the dependency graph of all the
	// nodes that affect this Scope, not just the ones provided directly to this Scope.
	gh *graphHolder

	// Parent of this Scope.
	parentScope *Scope

	// All the child scopes of this Scope.
	childScopes []*Scope

	// clockSrc stores the source of time. Defaults to system clock.
	clockSrc digclock.Clock
}

func newScope() *Scope {
	s := &Scope{
		providers:       make(map[key][]*constructorNode),
		decorators:      make(map[key]*decoratorNode),
		values:          make(map[key]reflect.Value),
		decoratedValues: make(map[key]reflect.Value),
		groups:          make(map[key][]reflect.Value),
		decoratedGroups: make(map[key]reflect.Value),
		invokerFn:       defaultInvoker,
		rand:            rand.New(rand.NewSource(time.Now().UnixNano())),
		clockSrc:        digclock.System,
	}
	s.gh = newGraphHolder(s)
	return s
}

// Scope creates a new Scope with the given name and options from current Scope.
// Any constructors that the current Scope knows about, as well as any modifications
// made to it in the future will be propagated to the child scope.
// However, no modifications made to the child scope being created will be propagated
// to the parent Scope.
func (s *Scope) Scope(name string, opts ...ScopeOption) *Scope {
	child := newScope()
	child.name = name
	child.parentScope = s
	child.invokerFn = s.invokerFn
	child.clockSrc = s.clockSrc
	child.deferAcyclicVerification = s.deferAcyclicVerification
	child.recoverFromPanics = s.recoverFromPanics

	// child copies the parent's graph nodes.
	for _, node := range s.gh.nodes {
		child.gh.nodes = append(child.gh.nodes, node)
		// Nodes must retain their order in the child scope. Otherwise,
		// nodes copied from the parent would report a different order in
		// the child, and cycle detection would report edges that do not
		// exist. This applies to value group nodes as well as
		// constructors: both participate in cycle detection.
		if orderer, ok := node.Wrapped.(interface {
			CopyOrder(parent, child *Scope)
		}); ok {
			orderer.CopyOrder(s, child)
		}
	}

	for _, opt := range opts {
		opt.noScopeOption()
	}

	s.childScopes = append(s.childScopes, child)
	return child
}

// ancestors returns a list of scopes of ancestors of this scope up to the
// root. The scope at at index 0 is this scope itself.
func (s *Scope) ancestors() []*Scope {
	var scopes []*Scope
	for s := s; s != nil; s = s.parentScope {
		scopes = append(scopes, s)
	}
	return scopes
}

func (s *Scope) appendSubscopes(dest []*Scope) []*Scope {
	dest = append(dest, s)
	for _, cs := range s.childScopes {
		dest = cs.appendSubscopes(dest)
	}
	return dest
}

func (s *Scope) storesToRoot() []containerStore {
	scopes := s.ancestors()
	stores := make([]containerStore, len(scopes))
	for i, s := range scopes {
		stores[i] = s
	}
	return stores
}

func (s *Scope) knownTypes() []reflect.Type {
	typeSet := make(map[reflect.Type]struct{}, len(s.providers))
	for k := range s.providers {
		typeSet[k.t] = struct{}{}
	}

	types := make([]reflect.Type, 0, len(typeSet))
	for t := range typeSet {
		types = append(types, t)
	}
	sort.Sort(byTypeName(types))
	return types
}

func (s *Scope) getValue(name string, t reflect.Type) (v reflect.Value, ok bool) {
	v, ok = s.values[key{name: name, t: t}]
	return
}

func (s *Scope) getDecoratedValue(name string, t reflect.Type) (v reflect.Value, ok bool) {
	v, ok = s.decoratedValues[key{name: name, t: t}]
	return
}

func (s *Scope) setValue(name string, t reflect.Type, v reflect.Value) {
	s.values[key{name: name, t: t}] = v
}

func (s *Scope) setDecoratedValue(name string, t reflect.Type, v reflect.Value) {
	s.decoratedValues[key{name: name, t: t}] = v
}

func (s *Scope) getValueGroup(name string, t reflect.Type) []reflect.Value {
	items := s.groups[key{group: name, t: t}]
	// shuffle the list so users don't rely on the ordering of grouped values
	return shuffledCopy(s.rand, items)
}

func (s *Scope) getDecoratedValueGroup(name string, t reflect.Type) (reflect.Value, bool) {
	items, ok := s.decoratedGroups[key{group: name, t: t}]
	return items, ok
}

func (s *Scope) submitGroupedValue(name string, t reflect.Type, v reflect.Value) {
	k := key{group: name, t: t}
	s.groups[k] = append(s.groups[k], v)
}

func (s *Scope) submitDecoratedGroupedValue(name string, t reflect.Type, v reflect.Value) {
	k := key{group: name, t: t}
	s.decoratedGroups[k] = v
}

func (s *Scope) getValueProviders(name string, t reflect.Type) []provider {
	return s.getProviders(key{name: name, t: t})
}

func (s *Scope) getGroupProviders(name string, t reflect.Type) []provider {
	return s.getProviders(key{group: name, t: t})
}

func (s *Scope) getValueDecorator(name string, t reflect.Type) (decorator, bool) {
	return s.getDecorators(key{name: name, t: t})
}

func (s *Scope) getGroupDecorator(name string, t reflect.Type) (decorator, bool) {
	return s.getDecorators(key{group: name, t: t})
}

func (s *Scope) getDecorators(k key) (decorator, bool) {
	d, found := s.decorators[k]
	return d, found
}

func (s *Scope) getProviders(k key) []provider {
	nodes := s.providers[k]
	providers := make([]provider, len(nodes))
	for i, n := range nodes {
		providers[i] = n
	}
	return providers
}

func (s *Scope) getAllGroupProviders(name string, t reflect.Type) []provider {
	return s.getAllProviders(key{group: name, t: t})
}

func (s *Scope) getAllValueProviders(name string, t reflect.Type) []provider {
	return s.getAllProviders(key{name: name, t: t})
}

func (s *Scope) getAllProviders(k key) []provider {
	allScopes := s.ancestors()
	var providers []provider
	for _, scope := range allScopes {
		providers = append(providers, scope.getProviders(k)...)
	}
	return providers
}

func (s *Scope) invoker() invokerFn {
	return s.invokerFn
}

func (s *Scope) clock() digclock.Clock {
	return s.clockSrc
}

// adds a new graphNode to this Scope and all of its descendent
// scope.
func (s *Scope) newGraphNode(wrapped interface{}, orders map[*Scope]int) {
	orders[s] = s.gh.NewNode(wrapped)
	for _, cs := range s.childScopes {
		cs.newGraphNode(wrapped, orders)
	}
}

func (s *Scope) cycleDetectedError(cycles [][]int) error {
	var paths [][]cycleErrPathEntry
	for _, cycle := range cycles {
		if path := s.cyclePath(cycle); len(path) > 0 {
			paths = append(paths, path)
		}
	}
	return errCycleDetected{Paths: paths, scope: s}
}

// cyclePath converts a cycle expressed as graph orders into the ordered list
// of constructors that participate in it. The first constructor is repeated
// at the end so that the path visibly closes.
//
// The cycle is rotated to start at its lowest constructor node so that the
// reported path is deterministic even when the cycle was discovered through
// a value group node. Value group nodes are omitted from the path because
// they do not correspond to user-provided functions.
func (s *Scope) cyclePath(cycle []int) []cycleErrPathEntry {
	if len(cycle) < 2 {
		return nil
	}

	// The last element repeats the first one.
	nodes := cycle[:len(cycle)-1]

	start := -1
	for i, n := range nodes {
		if _, ok := s.gh.Lookup(n).(*constructorNode); !ok {
			continue
		}
		if start < 0 || n < nodes[start] {
			start = i
		}
	}
	if start < 0 {
		return nil
	}

	rotated := make([]int, 0, len(nodes)+1)
	rotated = append(rotated, nodes[start:]...)
	rotated = append(rotated, nodes[:start]...)
	rotated = append(rotated, rotated[0])

	var path []cycleErrPathEntry
	for _, n := range rotated {
		node, ok := s.gh.Lookup(n).(*constructorNode)
		if !ok {
			continue
		}
		path = append(path, cycleErrPathEntry{
			Key:  key{t: node.CType()},
			Func: node.Location(),
		})
	}
	return path
}

// cycleSignature renders a stable identity for a cycle within the given
// Scope so that the same cycle observed through different Scopes is
// reported only once.
//
// Graph orders are per-Scope and can diverge when a Scope adds its own
// nodes, so the signature is based on node identity: the constructor node
// for constructor nodes and the group key for value group nodes.
func (s *Scope) cycleSignature(cycle []int) string {
	if len(cycle) == 0 {
		return ""
	}
	var b bytes.Buffer
	for i, n := range cycle {
		if i > 0 {
			b.WriteByte(',')
		}
		switch w := s.gh.Lookup(n).(type) {
		case *constructorNode:
			fmt.Fprintf(&b, "ctor:%v/%v", w.id, w.location)
		case *paramGroupedSlice:
			fmt.Fprintf(&b, "group:%v/%q", w.Type.Elem(), w.Group)
		default:
			fmt.Fprintf(&b, "node:%T", w)
		}
	}
	return b.String()
}

// Returns the root Scope that can be reached from this Scope.
func (s *Scope) rootScope() *Scope {
	curr := s
	for curr.parentScope != nil {
		curr = curr.parentScope
	}
	return curr
}

// String representation of the entire Scope
func (s *Scope) String() string {
	b := &bytes.Buffer{}
	fmt.Fprintln(b, "nodes: {")
	for k, vs := range s.providers {
		for _, v := range vs {
			fmt.Fprintln(b, "\t", k, "->", v)
		}
	}
	fmt.Fprintln(b, "}")

	fmt.Fprintln(b, "values: {")
	for k, v := range s.values {
		fmt.Fprintln(b, "\t", k, "=>", v)
	}
	for k, vs := range s.groups {
		for _, v := range vs {
			fmt.Fprintln(b, "\t", k, "=>", v)
		}
	}
	fmt.Fprintln(b, "}")

	return b.String()
}
