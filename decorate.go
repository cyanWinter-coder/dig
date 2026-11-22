// Copyright (c) 2022 Uber Technologies, Inc.
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

	"go.uber.org/dig/internal/digreflect"
	"go.uber.org/dig/internal/dot"
)

type decoratorState int

const (
	decoratorReady decoratorState = iota
	decoratorOnStack
	decoratorCalled
)

type decorator interface {
	Call(c containerStore) error
	ID() dot.CtorID
	State() decoratorState
}

type decoratorNode struct {
	dcor  interface{}
	dtype reflect.Type

	id dot.CtorID

	// Location where this function was defined.
	location *digreflect.Func

	// Current state of this decorator.
	state decoratorState

	// Generation of the graph at which this decorator last completed.
	// A decorator is applied at most once per type per generation, and is
	// applied again after the graph changes.
	calledAt int

	// Parameters of the decorator.
	params paramList

	// Results of the decorator.
	results resultList

	// Order of this node in each Scopes' graphHolders.
	orders map[*Scope]int

	// Scope this node was originally provided to.
	s *Scope

	// Callback for this decorator, if there is one.
	callback Callback

	// BeforeCallback for this decorator, if there is one
	beforeCallback BeforeCallback
}

func newDecoratorNode(dcor interface{}, s *Scope, opts decorateOptions) (*decoratorNode, error) {
	dval := reflect.ValueOf(dcor)
	dtype := dval.Type()
	dptr := dval.Pointer()

	pl, err := newParamList(dtype, s)
	if err != nil {
		return nil, err
	}

	rl, err := newResultList(dtype, resultOptions{})
	if err != nil {
		return nil, err
	}

	n := &decoratorNode{
		dcor:           dcor,
		dtype:          dtype,
		id:             dot.CtorID(dptr),
		location:       digreflect.InspectFunc(dcor),
		orders:         make(map[*Scope]int),
		params:         pl,
		results:        rl,
		s:              s,
		callback:       opts.Callback,
		beforeCallback: opts.BeforeCallback,
	}
	return n, nil
}

func (n *decoratorNode) Call(s containerStore) (err error) {
	gen := n.s.currentGeneration()
	if n.state == decoratorCalled && n.calledAt == gen {
		return nil
	}

	n.state = decoratorOnStack
	// If this call fails, leave the decorator ready so that a later
	// resolution can try again, instead of leaving it stuck on the stack.
	defer func() {
		if err != nil {
			n.state = decoratorReady
		}
	}()

	if err := shallowCheckDependencies(s, n.params); err != nil {
		return errMissingDependencies{
			Func:   n.location,
			Reason: err,
		}
	}

	args, err := n.params.BuildList(n.s)
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
		start := s.clock().Now()
		// Wrap in separate func to include PanicErrors
		defer func() {
			n.callback(CallbackInfo{
				Name:    fmt.Sprintf("%v.%v", n.location.Package, n.location.Name),
				Error:   err,
				Runtime: s.clock().Since(start),
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

	results := s.invoker()(reflect.ValueOf(n.dcor), args)
	// Buffer the decorated values so that a decorator returning an error
	// does not leave partial results behind. Otherwise a retry on the next
	// resolution would observe stale or empty decorated values.
	receiver := newStagingDecoratedContainerWriter()
	if err = n.results.ExtractList(receiver, true /* decorated */, results); err != nil {
		return err
	}
	receiver.Commit(n.s)
	n.state = decoratorCalled
	n.calledAt = gen
	return nil
}

func (n *decoratorNode) ID() dot.CtorID { return n.id }

func (n *decoratorNode) State() decoratorState {
	// A decorator from an older generation is treated as not called, so
	// that it is applied again against the current graph.
	if n.state == decoratorCalled && n.calledAt != n.s.currentGeneration() {
		return decoratorReady
	}
	return n.state
}

// DecorateOption modifies the default behavior of Decorate.
type DecorateOption interface {
	apply(*decorateOptions)
}

type decorateOptions struct {
	Info           *DecorateInfo
	Callback       Callback
	BeforeCallback BeforeCallback
}

// FillDecorateInfo is a DecorateOption that writes info on what Dig was
// able to get out of the provided decorator into the provided DecorateInfo.
func FillDecorateInfo(info *DecorateInfo) DecorateOption {
	return fillDecorateInfoOption{info: info}
}

type fillDecorateInfoOption struct{ info *DecorateInfo }

func (o fillDecorateInfoOption) String() string {
	return fmt.Sprintf("FillDecorateInfo(%p)", o.info)
}

func (o fillDecorateInfoOption) apply(opts *decorateOptions) {
	opts.Info = o.info
}

// DecorateInfo provides information about the decorator's inputs and outputs
// types as strings, as well as the ID of the decorator supplied to the Container.
type DecorateInfo struct {
	ID      ID
	Inputs  []*Input
	Outputs []*Output
}

// Decorate provides a decorator for a type that has already been provided in the Container.
// Decorations at this level affect all scopes of the container.
// See Scope.Decorate for information on how to use this method.
func (c *Container) Decorate(decorator interface{}, opts ...DecorateOption) error {
	return c.scope.Decorate(decorator, opts...)
}

// Decorate provides a decorator for a type that has already been provided in the Scope.
//
// Similar to Provide, Decorate takes in a function with zero or more dependencies and one
// or more results. Decorate can be used to modify a type that was already introduced to the
// Scope, or completely replace it with a new object.
//
// For example,
//
//	s.Decorate(func(log *zap.Logger) *zap.Logger {
//	  return log.Named("myapp")
//	})
//
// This takes in a value, augments it with a name, and returns a replacement for it. Functions
// in the Scope's dependency graph that use *zap.Logger will now use the *zap.Logger
// returned by this decorator.
//
// A decorator can also take in multiple parameters and replace one of them:
//
//	s.Decorate(func(log *zap.Logger, cfg *Config) *zap.Logger {
//	  return log.Named(cfg.Name)
//	})
//
// Or replace a subset of them:
//
//	s.Decorate(func(
//	  log *zap.Logger,
//	  cfg *Config,
//	  scope metrics.Scope
//	) (*zap.Logger, metrics.Scope) {
//	  log = log.Named(cfg.Name)
//	  scope = scope.With(metrics.Tag("service", cfg.Name))
//	  return log, scope
//	})
//
// Decorating a Scope affects all the child scopes of this Scope.
//
// Similar to a provider, the decorator function gets called *at most once*
// per type for a given version of the graph. If new providers or decorators
// are added to the Scope afterwards, the decorator is called again on the
// next resolution so that it observes the updated graph.
func (s *Scope) Decorate(decorator interface{}, opts ...DecorateOption) error {
	var options decorateOptions
	for _, opt := range opts {
		opt.apply(&options)
	}

	// Building the decorator node may add value group nodes to the graph
	// (via its parameters). If decorating is rejected, those nodes must not
	// remain in the candidate graph.
	allScopes := s.appendSubscopes(nil)
	for _, sc := range allScopes {
		sc.gh.Snapshot()
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		for _, sc := range allScopes {
			sc.gh.Rollback()
		}
	}()

	dn, err := newDecoratorNode(decorator, s, options)
	if err != nil {
		return err
	}

	keys, err := findResultKeys(dn.results)
	if err != nil {
		return err
	}

	// Validate every key before committing any of them, so that a decorator
	// that conflicts on one of its results does not leave the Scope
	// partially decorated.
	for _, k := range keys {
		if _, ok := s.decorators[k]; ok {
			return newErrInvalidInput(
				fmt.Sprintf("cannot decorate using function %v: %s already decorated", dn.dtype, k), nil)
		}
	}
	for _, k := range keys {
		s.decorators[k] = dn
	}
	committed = true

	// The graph changed: other decorators must be applied again on the next
	// resolution, and values produced by this one must not be reused from an
	// earlier generation.
	s.bumpGeneration()

	if info := options.Info; info != nil {
		params := dn.params.DotParam()
		results := dn.results.DotResult()
		info.ID = (ID)(dn.id)
		info.Inputs = make([]*Input, len(params))
		info.Outputs = make([]*Output, len(results))

		for i, param := range params {
			info.Inputs[i] = &Input{
				t:        param.Type,
				optional: param.Optional,
				name:     param.Name,
				group:    param.Group,
			}
		}
		for i, res := range results {
			info.Outputs[i] = &Output{
				t:     res.Type,
				name:  res.Name,
				group: res.Group,
			}
		}
	}
	return nil
}

func findResultKeys(r resultList) ([]key, error) {
	// use BFS to search for all keys included in a resultList.
	var (
		q    []result
		keys []key
	)
	q = append(q, r)

	for len(q) > 0 {
		res := q[0]
		q = q[1:]

		switch innerResult := res.(type) {
		case resultSingle:
			keys = append(keys, key{t: innerResult.Type, name: innerResult.Name})
		case resultGrouped:
			if innerResult.Type.Kind() != reflect.Slice {
				return nil, newErrInvalidInput("decorating a value group requires decorating the entire value group, not a single value", nil)
			}
			keys = append(keys, key{t: innerResult.Type.Elem(), group: innerResult.Group})
		case resultObject:
			for _, f := range innerResult.Fields {
				q = append(q, f.Result)
			}
		case resultList:
			q = append(q, innerResult.Results...)
		}
	}
	return keys, nil
}
