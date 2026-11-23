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
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/dig"
)

// invokeConcurrently runs fn in n goroutines and waits for all of them to
// finish, returning the error each goroutine received.
func invokeConcurrently(c *dig.Container, n int, fn interface{}) []error {
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = c.Invoke(fn)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

// TestConcurrentInvokeSingleConstruction verifies that when multiple
// goroutines resolve the same type concurrently, the constructor runs
// exactly once and every waiter shares the same committed instance.
func TestConcurrentInvokeSingleConstruction(t *testing.T) {
	t.Parallel()

	type result struct{ id int }

	c := dig.New()

	entered := make(chan struct{})
	release := make(chan struct{})
	var closeEntered sync.Once
	var calls int32

	require.NoError(t, c.Provide(func() *result {
		atomic.AddInt32(&calls, 1)
		// Announce the construction and hold it open so that the other
		// goroutines pile up on the in-flight call.
		closeEntered.Do(func() { close(entered) })
		<-release
		return &result{id: 42}
	}))

	const workers = 8
	instances := make([]*result, workers)
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.Invoke(func(r *result) { instances[i] = r })
		}(i)
	}

	<-entered
	// Give the remaining goroutines a chance to block on the in-flight
	// construction before letting it complete.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "goroutine %d", i)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls),
		"constructor must run exactly once under concurrent resolution")
	for i := 1; i < workers; i++ {
		assert.Same(t, instances[0], instances[i],
			"all waiters must share the same committed instance")
	}
}

// TestConcurrentInvokeFailureNotCached verifies that a failed construction
// leaves no entry in the container: the failure is not cached, and a later
// resolution re-runs the constructor.
func TestConcurrentInvokeFailureNotCached(t *testing.T) {
	t.Parallel()

	c := dig.New()

	var calls int32
	require.NoError(t, c.Provide(func() (int, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return 0, errors.New("first attempt fails")
		}
		return 42, nil
	}))

	// The first resolution fails; nothing may be committed to the
	// container.
	err := c.Invoke(func(int) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "first attempt fails")

	// The failure must not be cached: a subsequent resolution re-runs the
	// constructor and succeeds.
	require.NoError(t, c.Invoke(func(v int) {
		assert.Equal(t, 42, v)
	}))
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))

	// The now-cached value is served to concurrent resolutions without
	// re-running the constructor.
	errs := invokeConcurrently(c, 8, func(v int) {
		assert.Equal(t, 42, v)
	})
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls),
		"cached value must be reused, constructor must not run again")
}

// TestConcurrentInvokeFailureRetriedByWaiter verifies that waiters blocked
// on an in-flight construction that fails do not observe a poisoned
// container: the waiter retries the construction and reaches the success
// terminal state, while the caller of the failed attempt observes the
// failure. Both terminal states are consistent with a single commit
// boundary.
func TestConcurrentInvokeFailureRetriedByWaiter(t *testing.T) {
	t.Parallel()

	c := dig.New()

	entered := make(chan struct{})
	release := make(chan struct{})
	var closeEntered sync.Once
	var calls int32

	require.NoError(t, c.Provide(func() (string, error) {
		n := atomic.AddInt32(&calls, 1)
		closeEntered.Do(func() { close(entered) })
		<-release
		if n == 1 {
			return "", errors.New("first attempt fails")
		}
		return "ok", nil
	}))

	const workers = 2
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.Invoke(func(string) {})
		}(i)
	}

	// Wait for the first construction to be in flight, give the other
	// goroutine a chance to block on it, then let it fail.
	<-entered
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	var failures, successes int
	for _, err := range errs {
		if err != nil {
			failures++
			assert.Contains(t, err.Error(), "first attempt fails")
		} else {
			successes++
		}
	}
	assert.Equal(t, 1, failures, "exactly one caller observes the failed construction")
	assert.Equal(t, 1, successes, "the waiter retries and succeeds")
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))

	// The successful retry committed its value: no further constructions.
	require.NoError(t, c.Invoke(func(s string) {
		assert.Equal(t, "ok", s)
	}))
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

// TestConcurrentInvokePanicNotCached verifies that a panicking constructor
// (with RecoverFromPanics) leaves no entry in the container and may be
// retried.
func TestConcurrentInvokePanicNotCached(t *testing.T) {
	t.Parallel()

	c := dig.New(dig.RecoverFromPanics())

	var calls int32
	require.NoError(t, c.Provide(func() string {
		if atomic.AddInt32(&calls, 1) == 1 {
			panic("boom")
		}
		return "ok"
	}))

	err := c.Invoke(func(string) {})
	require.Error(t, err)
	var perr dig.PanicError
	require.ErrorAs(t, err, &perr)
	assert.Contains(t, err.Error(), "boom")

	// The panic must not be cached: a subsequent resolution re-runs the
	// constructor and succeeds.
	require.NoError(t, c.Invoke(func(s string) {
		assert.Equal(t, "ok", s)
	}))
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

// TestConcurrentInvokeGroupProviders verifies that group providers run
// exactly once under concurrent resolution and that grouped values are not
// duplicated in the container.
func TestConcurrentInvokeGroupProviders(t *testing.T) {
	t.Parallel()

	c := dig.New()

	var aCalls, bCalls int32
	require.NoError(t, c.Provide(func() string {
		atomic.AddInt32(&aCalls, 1)
		return "a"
	}, dig.Group("words")))
	require.NoError(t, c.Provide(func() string {
		atomic.AddInt32(&bCalls, 1)
		return "b"
	}, dig.Group("words")))

	type in struct {
		dig.In

		Words []string `group:"words"`
	}

	errs := invokeConcurrently(c, 8, func(in in) {
		assert.ElementsMatch(t, []string{"a", "b"}, in.Words)
	})
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&aCalls),
		"group provider must run exactly once")
	assert.Equal(t, int32(1), atomic.LoadInt32(&bCalls),
		"group provider must run exactly once")
}

// TestConcurrentInvokeNamedAndNested verifies concurrent resolution of a
// graph with named values and transitive dependencies.
func TestConcurrentInvokeNamedAndNested(t *testing.T) {
	t.Parallel()

	type conn struct{ dsn string }
	type repo struct{ c *conn }
	type handler struct{ r *repo }

	c := dig.New()

	var connCalls, repoCalls, handlerCalls int32
	require.NoError(t, c.Provide(func() *conn {
		atomic.AddInt32(&connCalls, 1)
		return &conn{dsn: "test"}
	}, dig.Name("primary")))

	type connIn struct {
		dig.In

		Conn *conn `name:"primary"`
	}
	require.NoError(t, c.Provide(func(in connIn) *repo {
		atomic.AddInt32(&repoCalls, 1)
		return &repo{c: in.Conn}
	}))
	require.NoError(t, c.Provide(func(r *repo) *handler {
		atomic.AddInt32(&handlerCalls, 1)
		return &handler{r: r}
	}))

	type in struct {
		dig.In

		Conn    *conn `name:"primary"`
		Handler *handler
	}

	errs := invokeConcurrently(c, 8, func(in in) {
		assert.Equal(t, "test", in.Conn.dsn)
		assert.Same(t, in.Conn, in.Handler.r.c)
	})
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&connCalls))
	assert.Equal(t, int32(1), atomic.LoadInt32(&repoCalls))
	assert.Equal(t, int32(1), atomic.LoadInt32(&handlerCalls))
}

// TestConcurrentDecorate verifies that a decorator runs exactly once under
// concurrent resolution of the decorated value.
func TestConcurrentDecorate(t *testing.T) {
	t.Parallel()

	c := dig.New()

	require.NoError(t, c.Provide(func() int { return 1 }))

	var decorateCalls int32
	require.NoError(t, c.Decorate(func(i int) int {
		atomic.AddInt32(&decorateCalls, 1)
		return i + 1
	}))

	errs := invokeConcurrently(c, 8, func(i int) {
		assert.Equal(t, 2, i)
	})
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&decorateCalls),
		"decorator must run exactly once under concurrent resolution")
}

// TestConcurrentInvokeMixedOutcomes verifies that independent resolutions
// racing on the same container do not affect each other: a failing
// resolution only fails its own call, and a successful resolution commits
// exactly once.
func TestConcurrentInvokeMixedOutcomes(t *testing.T) {
	t.Parallel()

	c := dig.New()

	var goodCalls int32
	require.NoError(t, c.Provide(func() int {
		atomic.AddInt32(&goodCalls, 1)
		return 1
	}))
	require.NoError(t, c.Provide(func() (string, error) {
		return "", fmt.Errorf("always fails")
	}))

	var wg sync.WaitGroup
	goodErrs := make([]error, 4)
	badErrs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			goodErrs[i] = c.Invoke(func(int) {})
		}(i)
		go func(i int) {
			defer wg.Done()
			badErrs[i] = c.Invoke(func(string) {})
		}(i)
	}
	wg.Wait()

	for i := range goodErrs {
		require.NoError(t, goodErrs[i], "good resolution %d must not be affected by the failing one", i)
		require.Error(t, badErrs[i])
		assert.Contains(t, badErrs[i].Error(), "always fails")
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&goodCalls),
		"successful constructor must commit exactly once")
}
