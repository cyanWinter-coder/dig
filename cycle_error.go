// Copyright (c) 2019 Uber Technologies, Inc.
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
	"errors"
	"fmt"
	"io"

	"go.uber.org/dig/internal/digreflect"
)

type cycleErrPathEntry struct {
	Key  key
	Func *digreflect.Func
}

type errCycleDetected struct {
	// Paths contains one path per detected cycle. Each path lists the
	// constructors of the cycle and repeats the first constructor at the
	// end, so that both the entry and the exit of the cycle are visible.
	//
	// Paths is deterministic: cycles are ordered by the graph order of
	// their constructors, and each path starts at its lowest constructor.
	Paths [][]cycleErrPathEntry
	scope *Scope
}

var _ digError = errCycleDetected{}

func (e errCycleDetected) Error() string {
	// We get something like,
	//
	//   [scope "foo"]
	//   func(*bar) *foo provided by "path/to/package".NewFoo (path/to/file.go:42)
	//   	depends on func(*baz) *bar provided by "another/package".NewBar (somefile.go:1)
	//   	depends on func(*foo) baz provided by "somepackage".NewBar (anotherfile.go:2)
	//   	depends on func(*bar) *foo provided by "path/to/package".NewFoo (path/to/file.go:42)
	//
	// If multiple independent cycles were found, each is listed in turn.
	// The rendering is bounded by the number of paths: it never follows
	// the graph, so it cannot recurse indefinitely.
	b := new(bytes.Buffer)

	if name := e.scope.name; len(name) > 0 {
		fmt.Fprintf(b, "[scope %q]\n", name)
	}
	for i, path := range e.Paths {
		if i > 0 {
			b.WriteString("\n")
		}
		for j, entry := range path {
			if j > 0 {
				b.WriteString("\n\tdepends on ")
			}
			fmt.Fprintf(b, "%v provided by %v", entry.Key, entry.Func)
		}
	}
	return b.String()
}

func (e errCycleDetected) writeMessage(w io.Writer, v string) {
	fmt.Fprint(w, e.Error())
}

func (e errCycleDetected) Format(w fmt.State, c rune) {
	formatError(e, w, c)
}

// IsCycleDetected returns a boolean as to whether the provided error indicates
// a cycle was detected in the container graph.
func IsCycleDetected(err error) bool {
	return errors.As(err, &errCycleDetected{})
}
