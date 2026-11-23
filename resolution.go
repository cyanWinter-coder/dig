// Copyright (c) 2024 Uber Technologies, Inc.
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
	"reflect"
	"sync"

	"go.uber.org/dig/internal/digclock"
	"go.uber.org/dig/internal/digerror"
	"go.uber.org/dig/internal/dot"
)

// resolution is the mutable state of a single Invoke.
//
// Every value published while resolving an Invoke is written through a
// resolutionStore, which records how to undo the write. If the dependencies
// of the invoked function cannot be built, the resolution is rolled back:
// every value, value group element, decorated value, and "already called"
// marker added by this Invoke is removed again. A Scope that fails therefore
// looks exactly like it did before Invoke, and neither its ancestors nor its
// siblings are affected.
type resolution struct {
	mu   sync.Mutex
	undo []func()
}

// record registers an undo step. Steps run in reverse order, so the oldest
// recorded state wins.
func (r *resolution) record(undo func()) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.undo = append(r.undo, undo)
	r.mu.Unlock()
}

// rollback undoes every recorded write.
func (r *resolution) rollback() {
	if r == nil {
		return
	}
	r.mu.Lock()
	undo := r.undo
	r.undo = nil
	r.mu.Unlock()

	for i := len(undo) - 1; i >= 0; i-- {
		undo[i]()
	}
}

// commit accepts the writes made by the resolution.
func (r *resolution) commit() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.undo = nil
	r.mu.Unlock()
}

// resolutionOf reports the resolution a containerStore is taking part in.
// It is nil when values are built outside of an Invoke.
func resolutionOf(c containerStore) *resolution {
	if rs, ok := c.(*resolutionStore); ok {
		return rs.res
	}
	return nil
}

// resolutionStore is the view of a Scope chain used for one resolution step.
//
// It is both the containerStore that parameters resolve against and the
// containerWriter that constructors and decorators publish through:
//
//   - Reads use a fixed chain of Scopes captured when the store was created.
//   - Writes are recorded for rollback and applied to the Scope that owns
//     the value being written.
//   - The chain does not change while the Invoke is in progress, so a
//     constructor, its parameters, its value groups, and its callbacks all
//     observe the same view of the graph.
type resolutionStore struct {
	// Scope is the view this store resolves against.
	*Scope

	// owner receives writes made through this store. It defaults to the
	// view's Scope. It differs only when values are resolved against a
	// different view, such as the originating Scope of an exported
	// constructor whose Scope is not visible to the requester.
	owner *Scope

	// chain is the fixed, nearest-first list of Scopes this store resolves
	// against. It always starts at the view Scope.
	chain []*Scope

	res *resolution
}

var _ containerStore = (*resolutionStore)(nil)
var _ containerWriter = (*resolutionStore)(nil)

// newResolutionStore builds a store that resolves against scope and its
// ancestors as they are right now.
func newResolutionStore(scope *Scope, res *resolution) *resolutionStore {
	return &resolutionStore{
		Scope: scope,
		owner: scope,
		chain: scope.ancestors(),
		res:   res,
	}
}

// newViewStore builds a store that resolves against scope's view of the
// graph but publishes values into owner.
func newViewStore(scope, owner *Scope, res *resolution) *resolutionStore {
	return &resolutionStore{
		Scope: scope,
		owner: owner,
		chain: scope.ancestors(),
		res:   res,
	}
}

// asResolutionStore returns c as a resolutionStore. A bare Scope, which
// internal callers and tests may pass, is wrapped without a rollback log.
func asResolutionStore(c containerStore) *resolutionStore {
	if rs, ok := c.(*resolutionStore); ok {
		return rs
	}
	scope, ok := c.(*Scope)
	if !ok {
		digerror.BugPanicf("unsupported containerStore %T", c)
	}
	return newResolutionStore(scope, nil)
}

func (rs *resolutionStore) ownerScope() *Scope {
	if rs.owner != nil {
		return rs.owner
	}
	return rs.Scope
}

func (rs *resolutionStore) storesToRoot() []containerStore {
	if rs.Scope == nil {
		return nil
	}
	stores := make([]containerStore, len(rs.chain))
	for i, s := range rs.chain {
		stores[i] = &resolutionStore{
			Scope: s,
			owner: rs.ownerScope(),
			chain: rs.chain[i:],
			res:   rs.res,
		}
	}
	return stores
}

// hasCalled reports whether any Scope on this store's fixed chain already
// constructed a value for the given constructor. The value produced by that
// call is visible through the chain, so it must not be built again.
func (rs *resolutionStore) hasCalled(n *constructorNode) bool {
	for _, s := range rs.chain {
		if s.hasCalled(n) {
			return true
		}
	}
	return false
}

func (rs *resolutionStore) setValue(name string, t reflect.Type, v reflect.Value) {
	rs.recordValue(key{name: name, t: t})
	rs.ownerScope().setValue(name, t, v)
}

func (rs *resolutionStore) setDecoratedValue(name string, t reflect.Type, v reflect.Value) {
	rs.recordDecoratedValue(key{name: name, t: t})
	rs.ownerScope().setDecoratedValue(name, t, v)
}

func (rs *resolutionStore) submitGroupedValue(name string, t reflect.Type, v reflect.Value) {
	rs.submitGroupedValueFrom(nil, name, t, v)
}

func (rs *resolutionStore) submitGroupedValueFrom(n *constructorNode, name string, t reflect.Type, v reflect.Value) {
	rs.recordGroup(key{group: name, t: t})
	rs.ownerScope().submitGroupedValueFrom(n, name, t, v)
}

func (rs *resolutionStore) submitDecoratedGroupedValue(name string, t reflect.Type, v reflect.Value) {
	rs.recordDecoratedGroup(key{group: name, t: t})
	rs.ownerScope().submitDecoratedGroupedValue(name, t, v)
}

func (rs *resolutionStore) recordValue(k key) {
	if rs.res == nil {
		return
	}
	owner := rs.ownerScope()
	prev, ok := owner.lookupValue(k)
	rs.res.record(func() { owner.restoreValue(k, prev, ok) })
}

func (rs *resolutionStore) recordDecoratedValue(k key) {
	if rs.res == nil {
		return
	}
	owner := rs.ownerScope()
	prev, ok := owner.lookupDecoratedValue(k)
	rs.res.record(func() { owner.restoreDecoratedValue(k, prev, ok) })
}

func (rs *resolutionStore) recordGroup(k key) {
	if rs.res == nil {
		return
	}
	owner := rs.ownerScope()
	prev, had := owner.lookupGroup(k)
	rs.res.record(func() { owner.restoreGroup(k, prev, had) })
}

func (rs *resolutionStore) recordDecoratedGroup(k key) {
	if rs.res == nil {
		return
	}
	owner := rs.ownerScope()
	prev, ok := owner.lookupDecoratedGroup(k)
	rs.res.record(func() { owner.restoreDecoratedGroup(k, prev, ok) })
}

// markCalled records on the Scope that owns the value that it finished
// constructing n. It reports false if that Scope had already recorded the
// call, in which case nothing is recorded for rollback.
func (rs *resolutionStore) markCalled(n *constructorNode) bool {
	owner := rs.ownerScope()
	if !owner.markCalled(n) {
		return false
	}
	rs.res.record(func() { owner.forgetCall(n) })
	return true
}

func (rs *resolutionStore) getValueGroupWithOwners(name string, t reflect.Type) ([]reflect.Value, []*constructorNode) {
	return rs.Scope.getValueGroupWithOwners(name, t)
}

func (rs *resolutionStore) knownTypes() []reflect.Type { return rs.Scope.knownTypes() }
func (rs *resolutionStore) createGraph() *dot.Graph    { return rs.Scope.createGraph() }
func (rs *resolutionStore) invoker() invokerFn         { return rs.Scope.invokerFn }
func (rs *resolutionStore) clock() digclock.Clock      { return rs.Scope.clockSrc }

func (rs *resolutionStore) newGraphNode(w interface{}, orders map[*Scope]int) {
	rs.Scope.newGraphNode(w, orders)
}

// collectGroup returns the visible elements of a value group, nearest Scope
// first.
//
// A constructor contributes all of its elements once. If a constructor's
// values exist in more than one Scope on the chain -- for example a parent
// built it for an earlier Invoke and the requester built its own copy -- only
// the nearest copy is collected. Elements produced by the same constructor
// within one Scope (a flattened slice) are all kept.
func collectGroup(c containerStore, group string, t reflect.Type) []reflect.Value {
	var (
		seen   = make(map[*constructorNode]struct{})
		result []reflect.Value
	)
	for _, store := range c.storesToRoot() {
		values, owners := store.getValueGroupWithOwners(group, t)

		// Constructors seen in this Scope are merged into seen only after
		// the whole Scope is collected, so a single constructor's multiple
		// elements are not collapsed.
		local := make(map[*constructorNode]struct{})
		for i, v := range values {
			if i < len(owners) && owners[i] != nil {
				n := owners[i]
				if _, ok := seen[n]; ok {
					continue
				}
				local[n] = struct{}{}
			}
			result = append(result, v)
		}
		for n := range local {
			seen[n] = struct{}{}
		}
	}
	return result
}
