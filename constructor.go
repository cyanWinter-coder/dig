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

	// Tracks the scopes for which the constructor owned by this node
	// was already called. A constructor is called at most once per
	// scope: the values it produces are committed to, and cached in,
	// the scope that triggered the call, so scopes that resolve the
	// same constructor independently get independent instances.
	called map[containerStore]bool

	// Type information about constructor parameters.
	paramList paramList

	// Type information about constructor results.
	resultList resultList

	// Order of this node in each Scopes' graphHolders.
	orders map[*Scope]int

	// Scope this node is part of.
	s *Scope

	// Scope this node was originally provided to.
	// This is different from s if and only if the constructor was Provided
	// with ExportOption.
	//
	// If the constructor is requested from a scope that origS is not an
	// ancestor of (only possible with Export), its dependencies are
	// resolved against origS's view of the graph, since they may refer to
	// constructors or decorators private to origS's subtree.
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
		called:         make(map[containerStore]bool),
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
func (n *constructorNode) Order(s *Scope) int         { return n.orders[s] }

// CopyOrder copies the order for the given parent scope to the given child scope.
func (n *constructorNode) CopyOrder(parent, child *Scope) {
	n.orders[child] = n.orders[parent]
}

func (n *constructorNode) String() string {
	return fmt.Sprintf("deps: %v, ctor: %v", n.paramList, n.ctype)
}

// Call calls this constructor if it hasn't already been called for the
// provided containerStore (or one of its ancestors) and injects any values
// produced by it into that containerStore.
//
// Values produced by the constructor are owned by the containerStore that
// triggered the call: they are cached in, and reused only within, that
// store and its descendants. A failed call commits nothing, so a scope
// that fails to resolve a constructor never pollutes its ancestors.
//
// The constructor's dependencies are resolved against the view of the
// graph seen by the triggering store. The only exception is a constructor
// that was Provided with Export from a scope that is not an ancestor of
// the triggering store: such a constructor's dependencies may be private
// to its originating scope's subtree, so they are resolved against the
// originating scope's view instead. In both cases, the constructor, its
// parameters (including value groups), and its callbacks observe a single
// consistent view of the graph within one call.
func (n *constructorNode) Call(c containerStore) (err error) {
	// view is the store whose graph the constructor resolves against.
	// It defaults to the triggering store, whose view includes everything
	// the constructor's originating scope can see.
	view := c

	// If this constructor was already called by this store or any of
	// its ancestors, the values it produced are already visible to this
	// store, so it must not be called again.
	called := false
	origVisible := false
	for _, s := range c.storesToRoot() {
		if n.called[s] {
			called = true
		}
		if s == n.origS {
			origVisible = true
		}
	}
	if called {
		return nil
	}
	if !origVisible {
		// The triggering store cannot see the constructor's originating
		// scope, so the constructor's dependencies may not be resolvable
		// from it. Resolve them against the originating scope's view.
		view = n.origS
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

	receiver := newStagingContainerWriter()
	results := view.invoker()(reflect.ValueOf(n.ctor), args)
	if err = n.resultList.ExtractList(receiver, false /* decorating */, results); err != nil {
		return errConstructorFailed{Func: n.location, Reason: err}
	}

	// Commit the results to the containerStore that triggered this call.
	// The values produced by the constructor belong to that store: they
	// are reused by it and its descendants, but are never visible to its
	// ancestors or siblings.
	receiver.Commit(c)
	n.called[c] = true
	return nil
}

// stagingContainerWriter is a containerWriter that records the changes that
// would be made to a containerWriter and defers them until Commit is called.
type stagingContainerWriter struct {
	values map[key]reflect.Value
	groups map[key][]reflect.Value
}

var _ containerWriter = (*stagingContainerWriter)(nil)

func newStagingContainerWriter() *stagingContainerWriter {
	return &stagingContainerWriter{
		values: make(map[key]reflect.Value),
		groups: make(map[key][]reflect.Value),
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

// Commit commits the received results to the provided containerWriter.
func (sr *stagingContainerWriter) Commit(cw containerWriter) {
	for k, v := range sr.values {
		cw.setValue(k.name, k.t, v)
	}

	for k, vs := range sr.groups {
		for _, v := range vs {
			cw.submitGroupedValue(k.group, k.t, v)
		}
	}
}
