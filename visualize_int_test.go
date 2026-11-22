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
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/dig/internal/dot"
)

func (c *Container) CreateGraph() *dot.Graph {
	return c.createGraph()
}

func (s *Scope) CreateGraph() *dot.Graph {
	return s.createGraph()
}

type nestedErr struct {
	err error
}

var _ digError = nestedErr{}

func (e nestedErr) Error() string {
	return fmt.Sprint(e)
}

func (e nestedErr) Unwrap() error {
	return e.err
}

func (e nestedErr) writeMessage(w io.Writer, _ string) {
	io.WriteString(w, "oh no")
}

func (e nestedErr) Format(w fmt.State, c rune) {
	formatError(e, w, c)
}

type visualizableErr struct{}

func (err visualizableErr) Error() string             { return "great sadness" }
func (err visualizableErr) updateGraph(dg *dot.Graph) {}

// recordingErr is an errVisualizer that records how many times it ran.
type recordingErr struct{ calls *int }

func (err recordingErr) Error() string { return "recorded" }
func (err recordingErr) updateGraph(*dot.Graph) {
	*err.calls++
}

func TestUpdateGraphWalksAllIndependentErrors(t *testing.T) {
	var first, second int
	dg := dot.NewGraph()

	err := newErrDiagnostics(
		errInvalidInput{Message: "missing", Cause: recordingErr{calls: &first}},
		errInvalidInput{Message: "cycle", Cause: recordingErr{calls: &second}},
	)
	requireIsDiagnostics(t, err)

	if err := updateGraph(dg, err); err != nil {
		t.Fatal(err)
	}

	// Both independent problems must be reflected in the graph; neither may
	// shadow the other.
	assert.Equal(t, 1, first, "first diagnostic must be applied")
	assert.Equal(t, 1, second, "second diagnostic must be applied")
}

func TestCanVisualizeErrorMultiError(t *testing.T) {
	err := newErrDiagnostics(
		errors.New("great sadness"),
		nestedErr{err: visualizableErr{}},
	)
	assert.True(t, CanVisualizeError(err),
		"a visualizable error nested in a multi-error must be found")
}

func TestErrDiagnostics(t *testing.T) {
	simple := errors.New("great sadness")

	t.Run("nil when empty", func(t *testing.T) {
		assert.NoError(t, newErrDiagnostics())
		assert.NoError(t, newErrDiagnostics(nil, nil))
	})

	t.Run("single error is returned as-is", func(t *testing.T) {
		assert.Equal(t, simple, newErrDiagnostics(nil, simple))
	})

	t.Run("multiple errors are all rendered", func(t *testing.T) {
		err := newErrDiagnostics(errors.New("first"), errors.New("second"))
		assert.Equal(t, "first; second", err.Error())
		assert.Equal(t, "first\nsecond", fmt.Sprintf("%+v", err))

		var ds errDiagnostics
		if !errors.As(err, &ds) {
			t.Fatalf("expected errDiagnostics, got %T", err)
		}
		assert.Len(t, ds.Unwrap(), 2)
	})

	t.Run("nested diagnostics are flattened", func(t *testing.T) {
		inner := newErrDiagnostics(errors.New("a"), errors.New("b"))
		err := newErrDiagnostics(inner, errors.New("c"))
		assert.Equal(t, "a; b; c", err.Error())
	})
}

// requireIsDiagnostics fails the test unless err is an errDiagnostics with
// more than one part.
func requireIsDiagnostics(t *testing.T, err error) {
	t.Helper()
	var ds errDiagnostics
	if !errors.As(err, &ds) {
		t.Fatalf("expected errDiagnostics, got %T", err)
	}
	if len(ds.Parts) < 2 {
		t.Fatalf("expected at least two parts, got %d", len(ds.Parts))
	}
}

func TestCanVisualizeError(t *testing.T) {
	tests := []struct {
		desc         string
		err          error
		canVisualize bool
	}{
		{
			desc:         "unvisualizable error",
			err:          errors.New("great sadness"),
			canVisualize: false,
		},
		{
			desc:         "nested unvisualizable error",
			err:          nestedErr{err: errors.New("great sadness")},
			canVisualize: false,
		},
		{
			desc:         "visualizable error",
			err:          visualizableErr{},
			canVisualize: true,
		},
		{
			desc:         "nested visualizable error",
			err:          nestedErr{err: visualizableErr{}},
			canVisualize: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			assert.Equal(t, tt.canVisualize, CanVisualizeError(tt.err))
		})
	}
}
