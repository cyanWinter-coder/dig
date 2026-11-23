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
	"sync"
	"sync/atomic"
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
//
// Values built while resolving a Scope belong to the Scope that triggered
// the construction. They are cached there, are visible to that Scope and
// its descendants, and are never published into an ancestor or a sibling
// Scope. A failed resolution leaves every Scope exactly as it was, and a
// closed Scope can be replaced by a new one that builds independent
// instances.
type Scope struct {
	// This implements containerStore interface.

	// mu guards the value caches, the record of completed constructor
	// calls, and the closed flag. Scopes never share it with their parent:
	// values constructed while resolving a Scope are written to that Scope
	// only, so independent request Scopes do not contend on the same lock.
	mu sync.RWMutex

	// treeMu guards the shape of the scope tree: childScopes and the copied
	// graph nodes. It is separate from mu so that creating a Scope does not
	// block resolutions that only read cached values.
	treeMu sync.RWMutex

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

	// Which constructor produced each element of groups, in the same order.
	// A nil entry is used for values whose producer is not known.
	groupOwners map[key][]*constructorNode

	// Values groups that generated via decoraters in the Scope.
	decoratedGroups map[key]reflect.Value

	// Constructor calls that this Scope has completed. A constructor runs
	// at most once per Scope: the instance it produces belongs to the
	// Scope that triggered the call and is reused only there and by its
	// descendants.
	calls map[*constructorNode]struct{}

	// closed is set by Close. A closed Scope keeps its graph, but it no
	// longer accepts new values or resolutions, and the values it cached
	// are released. A new Scope must be created for the next request.
	closed bool

	// Source of randomness.
	rand *rand.Rand

	// Flag indicating whether the graph has been checked for cycles.
	isVerifiedAcyclic atomic.Bool

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
		groupOwners:     make(map[key][]*constructorNode),
		decoratedGroups: make(map[key]reflect.Value),
		calls:           make(map[*constructorNode]struct{}),
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
	s.treeMu.RLock()
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
		if ctrNode, ok := node.Wrapped.(*constructorNode); ok {
			ctrNode.CopyOrder(s, child)
		}
	}
	s.treeMu.RUnlock()

	for _, opt := range opts {
		opt.noScopeOption()
	}

	s.treeMu.Lock()
	s.childScopes = append(s.childScopes, child)
	s.treeMu.Unlock()
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
	s.treeMu.RLock()
	children := make([]*Scope, len(s.childScopes))
	copy(children, s.childScopes)
	s.treeMu.RUnlock()
	for _, cs := range children {
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

// knownTypes returns all known types visible from this Scope. It walks the
// Scope chain so that a resolution against a fixed chain sees the same set of
// types that its providers were registered in.
func (s *Scope) knownTypes() []reflect.Type {
	typeSet := make(map[reflect.Type]struct{})
	for _, scope := range s.ancestors() {
		scope.mu.RLock()
		for k := range scope.providers {
			typeSet[k.t] = struct{}{}
		}
		scope.mu.RUnlock()
	}

	types := make([]reflect.Type, 0, len(typeSet))
	for t := range typeSet {
		types = append(types, t)
	}
	sort.Sort(byTypeName(types))
	return types
}

func (s *Scope) getValue(name string, t reflect.Type) (v reflect.Value, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok = s.values[key{name: name, t: t}]
	return
}

func (s *Scope) getDecoratedValue(name string, t reflect.Type) (v reflect.Value, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok = s.decoratedValues[key{name: name, t: t}]
	return
}

func (s *Scope) setValue(name string, t reflect.Type, v reflect.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key{name: name, t: t}] = v
}

func (s *Scope) setDecoratedValue(name string, t reflect.Type, v reflect.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decoratedValues[key{name: name, t: t}] = v
}

func (s *Scope) getValueGroup(name string, t reflect.Type) []reflect.Value {
	items, _ := s.getValueGroupWithOwners(name, t)
	return items
}

// getValueGroupWithOwners returns a shuffled copy of this Scope's value group
// together with the constructor that produced each element. The two slices
// are aligned: owners[i] produced items[i], before shuffling. A nil owner
// means the producer is not tracked.
func (s *Scope) getValueGroupWithOwners(name string, t reflect.Type) ([]reflect.Value, []*constructorNode) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key{group: name, t: t}
	items := s.groups[k]
	owners := s.groupOwners[k]

	perm := s.rand.Perm(len(items))
	newItems := make([]reflect.Value, len(items))
	newOwners := make([]*constructorNode, len(owners))
	for i, j := range perm {
		newItems[i] = items[j]
		if j < len(owners) {
			newOwners[i] = owners[j]
		}
	}
	return newItems, newOwners
}

func (s *Scope) getDecoratedValueGroup(name string, t reflect.Type) (reflect.Value, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items, ok := s.decoratedGroups[key{group: name, t: t}]
	return items, ok
}

func (s *Scope) submitGroupedValue(name string, t reflect.Type, v reflect.Value) {
	s.submitGroupedValueFrom(nil, name, t, v)
}

// submitGroupedValueFrom records v in this Scope's value group together with
// the constructor that produced it. The owner lets a later resolution collect
// each provider's element exactly once, even when the same provider also
// contributed an element further up the chain.
func (s *Scope) submitGroupedValueFrom(n *constructorNode, name string, t reflect.Type, v reflect.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key{group: name, t: t}
	s.groups[k] = append(s.groups[k], v)
	s.groupOwners[k] = append(s.groupOwners[k], n)
}

func (s *Scope) submitDecoratedGroupedValue(name string, t reflect.Type, v reflect.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key{group: name, t: t}
	s.decoratedGroups[k] = v
}

// lookupValue returns the cached value for k and whether it was present. It
// is used to capture an undo step.
func (s *Scope) lookupValue(k key) (reflect.Value, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[k]
	return v, ok
}

func (s *Scope) restoreValue(k key, v reflect.Value, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.values[k] = v
	} else {
		delete(s.values, k)
	}
}

func (s *Scope) lookupDecoratedValue(k key) (reflect.Value, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.decoratedValues[k]
	return v, ok
}

func (s *Scope) restoreDecoratedValue(k key, v reflect.Value, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.decoratedValues[k] = v
	} else {
		delete(s.decoratedValues, k)
	}
}

func (s *Scope) lookupGroup(k key) (groupSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values, ok := s.groups[k]
	if !ok {
		return groupSnapshot{}, false
	}
	owners := s.groupOwners[k]
	snap := groupSnapshot{
		values: append([]reflect.Value(nil), values...),
		owners: append([]*constructorNode(nil), owners...),
	}
	return snap, true
}

func (s *Scope) restoreGroup(k key, snap groupSnapshot, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.groups[k] = snap.values
		s.groupOwners[k] = snap.owners
	} else {
		delete(s.groups, k)
		delete(s.groupOwners, k)
	}
}

// groupSnapshot is a point-in-time copy of a value group and its owners.
type groupSnapshot struct {
	values []reflect.Value
	owners []*constructorNode
}

func (s *Scope) lookupDecoratedGroup(k key) (reflect.Value, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.decoratedGroups[k]
	return v, ok
}

func (s *Scope) restoreDecoratedGroup(k key, v reflect.Value, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.decoratedGroups[k] = v
	} else {
		delete(s.decoratedGroups, k)
	}
}

// hasCalled reports whether this Scope already constructed the value
// produced by n.
func (s *Scope) hasCalled(n *constructorNode) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.calls[n]
	return ok
}

// markCalled records that this Scope constructed the value produced by n.
// It reports false if the call was already recorded.
func (s *Scope) markCalled(n *constructorNode) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.calls[n]; ok {
		return false
	}
	s.calls[n] = struct{}{}
	return true
}

// forgetCall removes a recorded call. It is used to roll back a failed
// resolution.
func (s *Scope) forgetCall(n *constructorNode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.calls, n)
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, found := s.decorators[k]
	return d, found
}

func (s *Scope) getProviders(k key) []provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	s.treeMu.RLock()
	children := make([]*Scope, len(s.childScopes))
	copy(children, s.childScopes)
	s.treeMu.RUnlock()
	for _, cs := range children {
		cs.newGraphNode(wrapped, orders)
	}
}

// Close releases the values cached in this Scope.
//
// A request that used this Scope is over: the instances it constructed are
// no longer reachable through this Scope, and a new Scope must be created
// for the next request. Closing does not modify the parent Scope or sibling
// Scopes, so they keep serving the instances they already own.
//
// Close is idempotent. Invoking a closed Scope returns an error rather than
// stale instances, and a new Scope created from the same parent builds
// independent instances.
func (s *Scope) Close() error {
	s.mu.Lock()
	alreadyClosed := s.closed
	s.closed = true
	if !alreadyClosed {
		s.values = make(map[key]reflect.Value)
		s.decoratedValues = make(map[key]reflect.Value)
		s.groups = make(map[key][]reflect.Value)
		s.groupOwners = make(map[key][]*constructorNode)
		s.decoratedGroups = make(map[key]reflect.Value)
		s.calls = make(map[*constructorNode]struct{})
	}
	s.mu.Unlock()

	// Detach the closed Scope from its parent so nothing can reach it
	// through the tree again. The parent's providers, graph, and cached
	// values are left untouched.
	if parent := s.parentScope; parent != nil {
		parent.treeMu.Lock()
		for i, child := range parent.childScopes {
			if child == s {
				parent.childScopes = append(parent.childScopes[:i], parent.childScopes[i+1:]...)
				break
			}
		}
		parent.treeMu.Unlock()
	}
	return nil
}

// IsClosed reports whether Close was called on this Scope.
func (s *Scope) IsClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

func (s *Scope) cycleDetectedError(cycle []int) error {
	var path []cycleErrPathEntry
	for _, n := range cycle {
		if n, ok := s.gh.Lookup(n).(*constructorNode); ok {
			path = append(path, cycleErrPathEntry{
				Key: key{
					t: n.CType(),
				},
				Func: n.Location(),
			})
		}
	}
	return errCycleDetected{Path: path, scope: s}
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

	s.mu.RLock()
	defer s.mu.RUnlock()

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
