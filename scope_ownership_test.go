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

package dig_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/dig"
	"go.uber.org/dig/internal/digtest"
)

// These tests verify the ownership rules of scoped value resolution:
// instances constructed while resolving a Scope belong to that Scope,
// singletons are published only by the container that constructs them, and
// neither a failed resolution nor Close can modify a parent or sibling Scope.

func TestScopeOwnsConstructedValues(t *testing.T) {
	t.Parallel()

	type T struct{ n int }

	newContainer := func(t *testing.T) (*digtest.Container, func() int) {
		c := digtest.New(t)
		var calls int
		c.RequireProvide(func() *T {
			calls++
			return &T{n: calls}
		})
		return c, func() int { return calls }
	}

	t.Run("request scopes do not share instances", func(t *testing.T) {
		t.Parallel()

		c, calls := newContainer(t)
		first := c.Scope("request-1")
		second := c.Scope("request-2")

		var got1, again1, got2 *T
		require.NoError(t, first.Invoke(func(v *T) { got1 = v }))
		require.NoError(t, first.Invoke(func(v *T) { again1 = v }))
		require.NoError(t, second.Invoke(func(v *T) { got2 = v }))

		assert.Same(t, got1, again1, "a request must reuse the instance it constructed")
		assert.NotSame(t, got1, got2, "sibling requests must not share an instance")
		assert.Equal(t, 2, calls(), "each request must construct its own instance")
	})

	t.Run("request constructions are not published to the parent", func(t *testing.T) {
		t.Parallel()

		c, calls := newContainer(t)
		child := c.Scope("request")

		var childT, rootT, siblingT *T
		require.NoError(t, child.Invoke(func(v *T) { childT = v }))
		require.NoError(t, c.Invoke(func(v *T) { rootT = v }))
		sibling := c.Scope("sibling")
		require.NoError(t, sibling.Invoke(func(v *T) { siblingT = v }))

		assert.NotSame(t, childT, rootT, "the parent must construct its own singleton")
		assert.Same(t, rootT, siblingT,
			"a singleton published by the parent is reused by its descendants")
		assert.Equal(t, 2, calls())
	})

	t.Run("parent singleton is reused by later children", func(t *testing.T) {
		t.Parallel()

		c, calls := newContainer(t)
		var rootT *T
		require.NoError(t, c.Invoke(func(v *T) { rootT = v }))

		child := c.Scope("request")
		var childT *T
		require.NoError(t, child.Invoke(func(v *T) { childT = v }))

		assert.Same(t, rootT, childT, "a child must reuse a singleton its parent already published")
		assert.Equal(t, 1, calls(), "the parent singleton must be constructed once")
	})
}

func TestScopeFailureIsolation(t *testing.T) {
	t.Parallel()

	type Dep struct{ n int }
	type T struct{}

	t.Run("failed request leaves the parent cache untouched", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		var depCalls int
		c.RequireProvide(func() *Dep {
			depCalls++
			return &Dep{n: depCalls}
		})
		c.RequireProvide(func(*Dep) (*T, error) {
			return nil, errors.New("great sadness")
		})

		child := c.Scope("request")
		require.Error(t, child.Invoke(func(*T) {}))

		// *Dep was built while resolving the failed request. It belongs to
		// the request and must not appear in the parent or a sibling.
		var requestDep, rootDep, siblingDep *Dep
		require.NoError(t, child.Invoke(func(d *Dep) { requestDep = d }))
		require.NoError(t, c.Invoke(func(d *Dep) { rootDep = d }))
		sibling := c.Scope("sibling")
		require.NoError(t, sibling.Invoke(func(d *Dep) { siblingDep = d }))

		assert.NotSame(t, requestDep, rootDep)
		assert.NotSame(t, requestDep, siblingDep)
		assert.Equal(t, 3, depCalls, "each scope must construct its own dependency")
	})

	t.Run("a failed constructor is retried by the next resolution", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		var attempts int
		c.RequireProvide(func() (*T, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("transient")
			}
			return &T{}, nil
		})

		child := c.Scope("request")
		require.Error(t, child.Invoke(func(*T) {}))
		require.NoError(t, child.Invoke(func(*T) {}),
			"a failed construction must not be cached")
		assert.Equal(t, 2, attempts)
	})

	t.Run("failed group resolution commits nothing", func(t *testing.T) {
		t.Parallel()

		type out struct {
			dig.Out

			Value string `group:"values"`
		}
		type in struct {
			dig.In

			Values []string `group:"values"`
		}

		c := digtest.New(t)
		var goodCalls int
		c.RequireProvide(func() (out, error) {
			goodCalls++
			return out{Value: "good"}, nil
		})
		c.RequireProvide(func() (out, error) {
			return out{}, errors.New("bad element")
		})

		child := c.Scope("request")
		require.Error(t, child.Invoke(func(in) {}))
		require.Equal(t, 1, goodCalls)

		// The failed group left no partial elements behind: retrying the
		// whole group rebuilds the good element instead of reusing a cached
		// fragment from the failed attempt.
		require.Error(t, child.Invoke(func(in) {}))
		assert.Equal(t, 2, goodCalls,
			"a failed group must not cache the elements it built")
	})
}

func TestScopeGroupOwnership(t *testing.T) {
	t.Parallel()

	type in struct {
		dig.In

		Values []string `group:"values"`
	}

	t.Run("request group elements are not shared with siblings", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		var calls int
		c.RequireProvide(func() string {
			calls++
			return fmt.Sprintf("v%d", calls)
		}, dig.Group("values"))

		first := c.Scope("request-1")
		second := c.Scope("request-2")

		var firstValues, secondValues []string
		require.NoError(t, first.Invoke(func(i in) { firstValues = i.Values }))
		require.NoError(t, second.Invoke(func(i in) { secondValues = i.Values }))

		assert.Len(t, firstValues, 1)
		assert.Len(t, secondValues, 1)
		assert.NotEqual(t, firstValues, secondValues,
			"siblings must not share a group element built for a request")
		assert.Equal(t, 2, calls)
	})

	t.Run("a parent group element is reused by request scopes", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		var calls int
		c.RequireProvide(func() string {
			calls++
			return "shared"
		}, dig.Group("values"))

		var rootValues []string
		require.NoError(t, c.Invoke(func(i in) { rootValues = i.Values }))
		require.Equal(t, []string{"shared"}, rootValues)

		child := c.Scope("request")
		var childValues []string
		require.NoError(t, child.Invoke(func(i in) { childValues = i.Values }))

		assert.Equal(t, []string{"shared"}, childValues)
		assert.Equal(t, 1, calls, "the parent's element must not be built again")
	})

	t.Run("a group element appears once even when built by several scopes", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		var calls int
		c.RequireProvide(func() string {
			calls++
			return "value"
		}, dig.Group("values"))

		// Build the element as the parent.
		var rootValues []string
		require.NoError(t, c.Invoke(func(i in) { rootValues = i.Values }))

		// A request scope that already saw the parent's element must not
		// report it twice.
		child := c.Scope("request")
		var first, second []string
		require.NoError(t, child.Invoke(func(i in) { first = i.Values }))
		require.NoError(t, child.Invoke(func(i in) { second = i.Values }))
		assert.Equal(t, rootValues, first)
		assert.Equal(t, rootValues, second)
	})
}

func TestScopeClose(t *testing.T) {
	t.Parallel()

	type T struct{ n int }

	t.Run("close is idempotent and rejects invocation", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		c.RequireProvide(func() *T { return &T{} })

		child := c.Scope("request")
		require.NoError(t, child.Close())
		assert.True(t, child.IsClosed())
		require.NoError(t, child.Close(), "Close must be idempotent")

		err := child.Invoke(func(*T) {})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "can't invoke a closed scope")
	})

	t.Run("close does not modify the parent or siblings", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		var calls int
		c.RequireProvide(func() *T {
			calls++
			return &T{n: calls}
		})

		sibling := c.Scope("sibling")
		var siblingT *T
		require.NoError(t, sibling.Invoke(func(v *T) { siblingT = v }))

		closing := c.Scope("closing")
		var closingT *T
		require.NoError(t, closing.Invoke(func(v *T) { closingT = v }))
		require.NoError(t, closing.Close())

		var rootT, siblingAgain *T
		require.NoError(t, c.Invoke(func(v *T) { rootT = v }))
		require.NoError(t, sibling.Invoke(func(v *T) { siblingAgain = v }))

		assert.NotSame(t, closingT, rootT,
			"closing a request must not publish its instance to the parent")
		assert.Same(t, siblingT, siblingAgain,
			"closing a request must not disturb a sibling's instance")
	})

	t.Run("close detaches the request from the parent", func(t *testing.T) {
		t.Parallel()

		type RequestOnly struct{ n int }

		c := digtest.New(t)
		c.RequireProvide(func() *T { return &T{} })

		child := c.Scope("request")
		child.RequireProvide(func() *RequestOnly { return &RequestOnly{n: 1} })
		child.RequireInvoke(func(*RequestOnly) {})
		require.NoError(t, child.Close())

		// A closed request can no longer be resolved, and its private
		// constructors are gone from the container's graph.
		require.Error(t, child.Invoke(func(*RequestOnly) {}))
		var visualize strings.Builder
		require.NoError(t, dig.Visualize(c.Container, &visualize))
		assert.NotContains(t, visualize.String(), "RequestOnly",
			"a closed request's constructors must not remain in the parent graph")
	})

	t.Run("a scope recreated after close builds a fresh instance", func(t *testing.T) {
		t.Parallel()

		c := digtest.New(t)
		var calls int
		c.RequireProvide(func() *T {
			calls++
			return &T{n: calls}
		})

		first := c.Scope("request")
		var firstT *T
		require.NoError(t, first.Invoke(func(v *T) { firstT = v }))
		require.NoError(t, first.Close())

		second := c.Scope("request")
		var secondT *T
		require.NoError(t, second.Invoke(func(v *T) { secondT = v }))

		assert.NotSame(t, firstT, secondT,
			"a scope created after close must not return the old instance")
		assert.Equal(t, 2, calls)
	})
}

func TestScopeResolutionChainIsFixed(t *testing.T) {
	t.Parallel()

	t.Run("one Invoke resolves every dependency against the same chain", func(t *testing.T) {
		t.Parallel()

		type Config struct{ name string }
		type Service struct{ name string }

		c := digtest.New(t)
		c.RequireProvide(func() Config { return Config{name: "root"} })

		child := c.Scope("request")
		child.RequireDecorate(func(Config) Config { return Config{name: "request"} })
		child.RequireProvide(func(cfg Config) *Service { return &Service{name: cfg.name} })

		var seen string
		require.NoError(t, child.Invoke(func(s *Service) { seen = s.name }))
		assert.Equal(t, "request", seen,
			"a constructor must observe the same scope chain as the rest of its resolution")
	})
}

func TestScopeLifecycleRepeatable(t *testing.T) {
	t.Parallel()

	type T struct{ n int }

	// Exercise the request lifecycle several times to make sure the
	// ownership rules hold across repeated resolutions and recreations.
	c := digtest.New(t)
	var calls int
	c.RequireProvide(func() *T {
		calls++
		return &T{n: calls}
	})

	for i := 0; i < 5; i++ {
		request := c.Scope(fmt.Sprintf("request-%d", i))

		var first, second *T
		require.NoError(t, request.Invoke(func(v *T) { first = v }))
		require.NoError(t, request.Invoke(func(v *T) { second = v }))
		assert.Same(t, first, second, "a request must reuse its own instance")
		assert.Equal(t, i+1, calls)

		require.NoError(t, request.Close())
		assert.True(t, request.IsClosed())
	}

	var rootT *T
	require.NoError(t, c.Invoke(func(v *T) { rootT = v }))
	require.NotNil(t, rootT)
	assert.Equal(t, 6, calls, "the parent must construct its own singleton")
}

func TestScopeConcurrentRequests(t *testing.T) {
	t.Parallel()

	type T struct{ n int64 }

	c := digtest.New(t)
	var calls int64
	c.RequireProvide(func() *T {
		return &T{n: atomic.AddInt64(&calls, 1)}
	})

	const requests = 50
	var wg sync.WaitGroup
	errs := make(chan error, requests)

	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			request := c.Scope(fmt.Sprintf("request-%d", i))
			var first, second *T
			if err := request.Invoke(func(v *T) { first = v }); err != nil {
				errs <- err
				return
			}
			if err := request.Invoke(func(v *T) { second = v }); err != nil {
				errs <- err
				return
			}
			if first != second {
				errs <- errors.New("a request returned different instances")
				return
			}
			if err := request.Close(); err != nil {
				errs <- err
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}

	assert.EqualValues(t, requests, atomic.LoadInt64(&calls),
		"each request must construct exactly one instance")
}
