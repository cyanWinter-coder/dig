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

package dig_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/dig"
	"go.uber.org/dig/internal/digtest"
)

// TestDecorateSingleApplication verifies that a decorator is applied at
// most once per resolution generation: every consumer of the decorated
// type, within one resolution and across resolutions, observes the same
// decorated instance.
func TestDecorateSingleApplication(t *testing.T) {
	t.Parallel()

	c := digtest.New(t)
	type A struct{ Name string }
	type B struct{ A *A }

	c.RequireProvide(func() *A { return &A{Name: "original"} })
	c.RequireProvide(func(a *A) *B { return &B{A: a} })

	var calls int32
	decorated := &A{Name: "decorated"}
	c.RequireDecorate(func(a *A) *A {
		atomic.AddInt32(&calls, 1)
		return decorated
	})

	c.RequireInvoke(func(a *A, b *B) {
		assert.Same(t, decorated, a, "top-level consumer must see the decorated instance")
		assert.Same(t, decorated, b.A, "transitive consumer must see the same decorated instance")
	})
	c.RequireInvoke(func(a *A) {
		assert.Same(t, decorated, a, "later resolutions must reuse the decorated instance")
	})
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "decorator must run exactly once")
}

// TestDecorateFailureRecovery verifies that a failing decorator does not
// replace the original instance: its results are not committed, its state
// is rolled back, and the graph remains usable for later resolutions.
func TestDecorateFailureRecovery(t *testing.T) {
	t.Parallel()

	t.Run("failed decoration is not committed and is retried", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		type A struct{ Name string }
		type X struct{ Name string }

		c.RequireProvide(func() *A { return &A{Name: "original"} })
		c.RequireProvide(func() *X { return &X{Name: "unrelated"} })

		var calls int32
		c.RequireDecorate(func(a *A) (*A, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				// Return a non-nil value alongside the error: it must
				// NOT be committed to the container.
				return &A{Name: "corrupt"}, errors.New("great sadness")
			}
			return &A{Name: "decorated"}, nil
		})

		err := c.Invoke(func(a *A) {})
		require.Error(t, err, "expected the first resolution to fail")
		assert.Contains(t, err.Error(), "great sadness")

		// The old graph remains usable for unrelated types.
		c.RequireInvoke(func(x *X) {
			assert.Equal(t, "unrelated", x.Name)
		})

		// The failed decorator is retried by the next resolution, and the
		// partially returned value from the failed call was never committed.
		c.RequireInvoke(func(a *A) {
			assert.Equal(t, "decorated", a.Name)
		})
		assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "decorator must be retried after failure")
	})

	t.Run("failed multi-result decoration commits nothing", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		type A struct{ Name string }
		type B struct{ Name string }

		c.RequireProvide(func() *A { return &A{Name: "originalA"} })
		c.RequireProvide(func() *B { return &B{Name: "originalB"} })

		var calls int32
		c.RequireDecorate(func(a *A, b *B) (*A, *B, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return &A{Name: "corruptA"}, &B{Name: "corruptB"}, errors.New("great sadness")
			}
			return &A{Name: "decoratedA"}, &B{Name: "decoratedB"}, nil
		})

		err := c.Invoke(func(a *A, b *B) {})
		require.Error(t, err, "expected the first resolution to fail")

		c.RequireInvoke(func(a *A, b *B) {
			assert.Equal(t, "decoratedA", a.Name)
			assert.Equal(t, "decoratedB", b.Name)
		})
		assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
	})

	t.Run("failed group decoration is retried", func(t *testing.T) {
		t.Parallel()

		type DecorateIn struct {
			dig.In
			Values []string `group:"value"`
		}
		type DecorateOut struct {
			dig.Out
			Values []string `group:"value"`
		}
		type InvokeIn struct {
			dig.In
			Values []string `group:"value"`
		}

		c := digtest.New(t)
		c.RequireProvide(func() string { return "value" }, dig.Group("value"))

		var calls int32
		c.RequireDecorate(func(i DecorateIn) (DecorateOut, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return DecorateOut{Values: []string{"corrupt"}}, errors.New("great sadness")
			}
			return DecorateOut{Values: append(i.Values, "decorated")}, nil
		})

		err := c.Invoke(func(InvokeIn) {})
		require.Error(t, err, "expected the first resolution to fail")

		c.RequireInvoke(func(i InvokeIn) {
			assert.ElementsMatch(t, []string{"value", "decorated"}, i.Values)
		})
		assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
	})
}

// TestDecorateFrozenDuringResolution verifies that the decoration chain is
// frozen once a resolution begins: a Decorate issued while a resolution is
// in progress takes effect only for subsequent resolutions.
func TestDecorateFrozenDuringResolution(t *testing.T) {
	t.Parallel()

	c := dig.New()
	type A struct{ Name string }
	require.NoError(t, c.Provide(func() *A { return &A{Name: "original"} }))

	started := make(chan struct{})
	release := make(chan struct{})
	invokeResult := make(chan string, 1)
	invokeDone := make(chan error, 1)
	go func() {
		invokeDone <- c.Invoke(func(a *A) {
			close(started)
			<-release
			invokeResult <- a.Name
		})
	}()
	<-started

	decorateDone := make(chan error, 1)
	go func() {
		decorateDone <- c.Decorate(func(a *A) *A { return &A{Name: "decorated"} })
	}()

	// The Decorate call is published after the in-flight resolution: it
	// must not complete, nor take effect, while the resolution is frozen.
	select {
	case err := <-decorateDone:
		t.Fatalf("Decorate completed during an in-flight resolution: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-invokeDone)
	assert.Equal(t, "original", <-invokeResult,
		"in-flight resolution must observe the pre-Decorate graph")

	require.NoError(t, <-decorateDone)

	// The next resolution observes the decoration.
	require.NoError(t, c.Invoke(func(a *A) {
		assert.Equal(t, "decorated", a.Name)
	}))
}

// TestDecorateConcurrent verifies that concurrent Decorate and resolution
// are published in a deterministic order, and that conflicting Decorate
// calls fail with an error naming the conflicting type.
func TestDecorateConcurrent(t *testing.T) {
	t.Parallel()

	t.Run("concurrent duplicate Decorate fails with the conflicting type", func(t *testing.T) {
		t.Parallel()

		c := dig.New()
		type A struct{ Name string }
		require.NoError(t, c.Provide(func() *A { return &A{Name: "original"} }))

		const goroutines = 8
		errs := make([]error, goroutines)
		barrier := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-barrier
				errs[i] = c.Decorate(func(a *A) *A { return &A{Name: "decorated"} })
			}(i)
		}
		close(barrier)
		wg.Wait()

		var successes int
		for _, err := range errs {
			if err == nil {
				successes++
				continue
			}
			assert.Contains(t, err.Error(), "already decorated")
			assert.Contains(t, err.Error(), "*dig_test.A", "error must name the conflicting type")
		}
		assert.Equal(t, 1, successes, "exactly one Decorate must win")
	})

	t.Run("concurrent resolutions apply the decorator exactly once", func(t *testing.T) {
		t.Parallel()

		c := dig.New()
		type A struct{ Name string }
		require.NoError(t, c.Provide(func() *A { return &A{Name: "original"} }))

		var calls int32
		require.NoError(t, c.Decorate(func(a *A) *A {
			atomic.AddInt32(&calls, 1)
			return &A{Name: "decorated"}
		}))

		const goroutines = 16
		errs := make([]error, goroutines)
		names := make([]string, goroutines)
		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = c.Invoke(func(a *A) { names[i] = a.Name })
			}(i)
		}
		wg.Wait()

		for i := range errs {
			require.NoError(t, errs[i])
			assert.Equal(t, "decorated", names[i],
				"every concurrent resolution must observe the decorated instance")
		}
		assert.Equal(t, int32(1), atomic.LoadInt32(&calls),
			"decorator must be applied exactly once across concurrent resolutions")
	})

	t.Run("concurrent Decorate and Invoke are serialized", func(t *testing.T) {
		t.Parallel()

		c := dig.New()
		type A struct{ Name string }
		require.NoError(t, c.Provide(func() *A { return &A{Name: "original"} }))

		var calls int32
		decorateStarted := make(chan struct{})
		decorateDone := make(chan error, 1)
		go func() {
			close(decorateStarted)
			decorateDone <- c.Decorate(func(a *A) *A {
				atomic.AddInt32(&calls, 1)
				return &A{Name: "decorated"}
			})
		}()
		<-decorateStarted

		const goroutines = 16
		errs := make([]error, goroutines)
		names := make([]string, goroutines)
		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = c.Invoke(func(a *A) { names[i] = a.Name })
			}(i)
		}
		wg.Wait()

		// Each resolution observes either the graph before the Decorate or
		// after it -- never a mix -- and once the Decorate is published all
		// resolutions observe the decorated instance.
		for i := range errs {
			require.NoError(t, errs[i])
			assert.Contains(t, []string{"original", "decorated"}, names[i])
		}
		assert.LessOrEqual(t, atomic.LoadInt32(&calls), int32(1))

		// Once the Decorate is published, all resolutions observe the
		// decorated instance.
		require.NoError(t, <-decorateDone)
		require.NoError(t, c.Invoke(func(a *A) {
			assert.Equal(t, "decorated", a.Name)
		}))
	})
}
