// Copyright (c) 2026 Uber Technologies, Inc.
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
)

// groupTxKey identifies a transactional value group build: the group being
// built and the leaf scope whose resolution path triggered the build.
type groupTxKey struct {
	leaf  *Scope
	group key
}

// groupTransaction is a transactional build of a single value group.
//
// While a groupTransaction is active, results of constructors that run as
// part of the build are staged in the transaction instead of being
// committed to their scopes, and reads of the affected scopes observe the
// staged values. This gives the whole resolution one consistent version of
// the graph: group parameters and regular parameters resolved while the
// transaction is active see the same staged and committed state.
//
// If every element of the group builds successfully, commit applies all
// staged results to their scopes in the order the constructors completed,
// and only then is the group delivered. If any element fails, the
// transaction is discarded: no partial results become visible and no
// constructor is marked as called, so the next resolution retries the
// whole group.
//
// Concurrent resolutions of the same group share a single
// groupTransaction: the first one builds, and the others wait on done and
// observe the same outcome.
type groupTransaction struct {
	// done is closed when the build completes, successfully or not.
	done chan struct{}

	// err is the failure of the build, if any. It is set before done is
	// closed.
	err error

	// panicked, if non-nil, is a panic that escaped the build. Waiters
	// re-panic with it so that all sharers of the build observe the same
	// outcome.
	panicked interface{}

	// commits holds the staged constructor results in the order the
	// constructors completed.
	commits []*stagedCommit

	// called tracks constructors that already ran as part of this
	// transaction so they are not run twice.
	called map[*constructorNode]struct{}
}

// stagedCommit is a single constructor's results staged for commit to a
// specific scope.
type stagedCommit struct {
	node   *constructorNode
	scope  *Scope
	writer *stagingContainerWriter
}

func newGroupTransaction() *groupTransaction {
	return &groupTransaction{
		done:   make(chan struct{}),
		called: make(map[*constructorNode]struct{}),
	}
}

// isCalled reports whether the constructor already ran as part of this
// transaction.
func (tx *groupTransaction) isCalled(n *constructorNode) bool {
	_, ok := tx.called[n]
	return ok
}

// stage records the results of a constructor that ran as part of this
// transaction. The results become visible to readers in this transaction
// immediately, and to everyone else only after commit.
func (tx *groupTransaction) stage(n *constructorNode, s *Scope, w *stagingContainerWriter) {
	tx.commits = append(tx.commits, &stagedCommit{node: n, scope: s, writer: w})
	tx.called[n] = struct{}{}
}

// commit applies all staged results to their scopes in the order the
// constructors completed and marks the constructors as called.
func (tx *groupTransaction) commit() {
	for _, c := range tx.commits {
		c.writer.Commit(c.scope)
		c.node.called = true
	}
}

// stagedValue returns the most recently staged value for the given key in
// the given scope, if any.
func (tx *groupTransaction) stagedValue(s *Scope, k key) (reflect.Value, bool) {
	for i := len(tx.commits) - 1; i >= 0; i-- {
		c := tx.commits[i]
		if c.scope != s {
			continue
		}
		if v, ok := c.writer.values[k]; ok {
			return v, true
		}
	}
	return _noValue, false
}

// stagedGroupValues returns all values staged for the given group key in
// the given scope, in the order they were produced.
func (tx *groupTransaction) stagedGroupValues(s *Scope, k key) []reflect.Value {
	var vs []reflect.Value
	for _, c := range tx.commits {
		if c.scope != s {
			continue
		}
		vs = append(vs, c.writer.groups[k]...)
	}
	return vs
}
