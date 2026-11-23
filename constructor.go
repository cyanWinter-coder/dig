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
	"fmt"
	"reflect"
	"sync"

	"go.uber.org/dig/internal/digerror"
	"go.uber.org/dig/internal/digreflect"
	"go.uber.org/dig/internal/dot"
)

// constructorNode is a node in the dependency graph that represents
// a constructor provided by the user.
//
// constructorNodes can produce zero or more values that they store into the container.
// For the Provide path, we verify that constructorNodes produce at least one value,
// otherwise the function will never be called.
type constructorNode struct {
	ctor  interface{}
	ctype reflect.Type

	// Location where this function was defined.
	location *digreflect.Func

	// id uniquely identifies the constructor that produces a node.
	id dot.CtorID

	// Type information about constructor parameters.
	paramList paramList

	// Type information about constructor results.
	resultList resultList

	// Order of this node in each Scopes' graphHolders. Guarded by
	// ordersMu because creating a child Scope copies the order into a new
	// entry while other goroutines may be creating sibling Scopes.
	ordersMu sync.RWMutex
	orders   map[*Scope]int

	// Scope this node is part of.
	s *Scope

	// Scope this node was originally provided to.
	// This is different from s if and only if the constructor was Provided with ExportOption.
	origS *Scope

	// Callback for this provided function, if there is one.
	callback Callback

	// BeforeCallback for this provided function, if there is one.
	beforeCallback BeforeCallback
}

type constructorOptions struct {
	// If specified, all values produced by this constructor have the provided name
	// belong to the specified value group or implement any of the interfaces.
	ResultName     string
	ResultGroup    string
	ResultAs       []interface{}
	Location       *digreflect.Func
	Callback       Callback
	BeforeCallback BeforeCallback
}

func newConstructorNode(ctor interface{}, s *Scope, origS *Scope, opts constructorOptions) (*constructorNode, error) {
	cval := reflect.ValueOf(ctor)
	ctype := cval.Type()
	cptr := cval.Pointer()

	params, err := newParamList(ctype, s)
	if err != nil {
		return nil, err
	}

	results, err := newResultList(
		ctype,
		resultOptions{
			Name:  opts.ResultName,
			Group: opts.ResultGroup,
			As:    opts.ResultAs,
		},
	)
	if err != nil {
		return nil, err
	}

	location := opts.Location
	if location == nil {
		location = digreflect.InspectFunc(ctor)
	}

	n := &constructorNode{
		ctor:           ctor,
		ctype:          ctype,
		location:       location,
		id:             dot.CtorID(cptr),
		paramList:      params,
		resultList:     results,
		orders:         make(map[*Scope]int),
		s:              s,
		origS:          origS,
		callback:       opts.Callback,
		beforeCallback: opts.BeforeCallback,
	}
	s.newGraphNode(n, n.orders)
	return n, nil
}

func (n *constructorNode) Location() *digreflect.Func { return n.location }
func (n *constructorNode) ParamList() paramList       { return n.paramList }
func (n *constructorNode) ResultList() resultList     { return n.resultList }
func (n *constructorNode) ID() dot.CtorID             { return n.id }
func (n *constructorNode) CType() reflect.Type        { return n.ctype }
func (n *constructorNode) Order(s *Scope) int {
	n.ordersMu.RLock()
	defer n.ordersMu.RUnlock()
	return n.orders[s]
}
func (n *constructorNode) OrigScope() *Scope { return n.origS }

// CopyOrder copies the order for the given parent scope to the given child scope.
func (n *constructorNode) CopyOrder(parent, child *Scope) {
	n.ordersMu.Lock()
	defer n.ordersMu.Unlock()
	n.orders[child] = n.orders[parent]
}

func (n *constructorNode) String() string {
	return fmt.Sprintf("deps: %v, ctor: %v", n.paramList, n.ctype)
}

// Call calls this constructor if it has not already been called on the
// resolving Scope chain, and injects the values it produces into the Scope
// that triggered the resolution.
//
// The instance a constructor produces belongs to the Scope that triggered the
// resolution: it is cached there and reused only by that Scope and its
// descendants. It is never published into an ancestor or a sibling Scope.
// Only a Scope that resolves the constructor itself publishes the resulting
// singleton, so an ancestor that resolves first shares its instance with its
// descendants, while independent request Scopes each get their own.
//
// Dependencies are resolved against the fixed view the triggering store
// already has for the duration of an Invoke. A constructor provided with
// Export from a Scope the request cannot see resolves its dependencies
// against the Scope it was declared in, since they may be private there, but
// the produced instance still belongs to the request.
//
// If any step fails, nothing is published: the caller rolls the whole
// resolution back, so a failed constructor leaves every Scope unchanged.
func (n *constructorNode) Call(c containerStore) (err error) {
	store := asResolutionStore(c)

	// The constructor runs at most once per Scope. If a Scope on the fixed
	// chain already produced the value, it is visible to the request and
	// must not be built again.
	if store.hasCalled(n) {
		return nil
	}

	// view is the store whose graph the constructor resolves against, and
	// publisher is the Scope that receives the produced values.
	//
	// For a constructor provided to the resolving Scope, both are the
	// requesting Scope: the instance belongs to the request and is never
	// shared with an ancestor or a sibling.
	//
	// For an exported constructor, the instance is a singleton published
	// by the Scope it was exported to, so every Scope that sees that Scope
	// reuses it. Its dependencies, however, resolve against the Scope it
	// was declared in, since they may be private to that Scope.
	view := store
	publisher := store
	if n.origS != n.s {
		view = newResolutionStore(n.origS, store.res)
		publisher = newViewStore(n.origS, n.s, store.res)
	}

	if err := shallowCheckDependencies(view, n.paramList); err != nil {
		return errMissingDependencies{
			Func:   n.location,
			Reason: err,
		}
	}

	args, err := n.paramList.BuildList(view)
	if err != nil {
		return errArgumentsFailed{
			Func:   n.location,
			Reason: err,
		}
	}

	if n.beforeCallback != nil {
		n.beforeCallback(BeforeCallbackInfo{
			Name: fmt.Sprintf("%v.%v", n.location.Package, n.location.Name),
		})
	}

	if n.callback != nil {
		start := view.clock().Now()
		// Wrap in separate func to include PanicErrors
		defer func() {
			n.callback(CallbackInfo{
				Name:    fmt.Sprintf("%v.%v", n.location.Package, n.location.Name),
				Error:   err,
				Runtime: view.clock().Since(start),
			})
		}()
	}

	if n.s.recoverFromPanics {
		defer func() {
			if p := recover(); p != nil {
				err = PanicError{
					fn:    n.location,
					Panic: p,
				}
			}
		}()
	}

	// Extract results into a staging writer first: a constructor that
	// returns an error, or whose results cannot be extracted, publishes
	// nothing at all.
	receiver := newStagingContainerWriter(n)
	results := view.invoker()(reflect.ValueOf(n.ctor), args)
	if err = n.resultList.ExtractList(receiver, false /* decorating */, results); err != nil {
		return errConstructorFailed{Func: n.location, Reason: err}
	}

	// Record the call on the Scope that owns these values before
	// publishing them. If another resolution already completed the
	// constructor for that Scope, it owns the instance: drop the duplicate
	// results instead of overwriting the published value.
	if !publisher.markCalled(n) {
		return nil
	}
	// Publish the results. The write is journaled by the resolution so
	// that a later failure undoes it together with the recorded call.
	receiver.Commit(publisher)
	return nil
}

// stagingContainerWriter is a containerWriter that records the values
// produced by a constructor and defers them until Commit is called.
//
// Recording lets a constructor that fails partway through publish nothing.
// The values are written to the target only on Commit, together with the
// constructor that produced them, so value groups can attribute each element
// to its producer.
type stagingContainerWriter struct {
	values map[key]reflect.Value
	groups map[key][]reflect.Value

	// ctor is the constructor whose results are being staged. It is
	// recorded as the owner of any value group elements.
	ctor *constructorNode
}

var _ containerWriter = (*stagingContainerWriter)(nil)

func newStagingContainerWriter(ctor *constructorNode) *stagingContainerWriter {
	return &stagingContainerWriter{
		values: make(map[key]reflect.Value),
		groups: make(map[key][]reflect.Value),
		ctor:   ctor,
	}
}

func (sr *stagingContainerWriter) setValue(name string, t reflect.Type, v reflect.Value) {
	sr.values[key{t: t, name: name}] = v
}

func (sr *stagingContainerWriter) setDecoratedValue(_ string, _ reflect.Type, _ reflect.Value) {
	digerror.BugPanicf("stagingContainerWriter.setDecoratedValue must never be called")
}

func (sr *stagingContainerWriter) submitGroupedValue(group string, t reflect.Type, v reflect.Value) {
	k := key{t: t, group: group}
	sr.groups[k] = append(sr.groups[k], v)
}

func (sr *stagingContainerWriter) submitDecoratedGroupedValue(_ string, _ reflect.Type, _ reflect.Value) {
	digerror.BugPanicf("stagingContainerWriter.submitDecoratedGroupedValue must never be called")
}

// Commit commits the staged results to the provided containerWriter.
func (sr *stagingContainerWriter) Commit(cw containerWriter) {
	for k, v := range sr.values {
		cw.setValue(k.name, k.t, v)
	}

	ownerWriter, recordsOwner := cw.(interface {
		submitGroupedValueFrom(*constructorNode, string, reflect.Type, reflect.Value)
	})
	for k, vs := range sr.groups {
		for _, v := range vs {
			if recordsOwner {
				ownerWriter.submitGroupedValueFrom(sr.ctor, k.group, k.t, v)
				continue
			}
			cw.submitGroupedValue(k.group, k.t, v)
		}
	}
}
