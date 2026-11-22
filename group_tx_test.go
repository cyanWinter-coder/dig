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
	"errors"
	"math/rand"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Concurrent resolutions of the same value group must share a single
// transactional build: the providers run exactly once and every resolution
// observes the same resulting group.
func TestGroupBuildConcurrentSharedResult(t *testing.T) {
	c := New(setRand(rand.New(rand.NewSource(0))))
	s := c.scope

	type out struct {
		Out

		V string `group:"x"`
	}

	release := make(chan struct{})
	var calls int32
	for _, v := range []string{"a", "b", "c"} {
		v := v
		require.NoError(t, s.Provide(func() out {
			atomic.AddInt32(&calls, 1)
			<-release // hold the build open so resolutions overlap
			return out{V: v}
		}))
	}

	type in struct {
		In

		Strings []string `group:"x"`
	}
	// Build the parameter list once, up front, so that the concurrent
	// resolutions below exercise only the group build.
	pl, err := newParamList(reflect.TypeOf(func(in) {}), s)
	require.NoError(t, err)

	const numResolutions = 8
	results := make([][]string, numResolutions)
	errs := make([]error, numResolutions)
	var wg sync.WaitGroup
	for i := 0; i < numResolutions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			args, err := pl.BuildList(s)
			if err != nil {
				errs[i] = err
				return
			}
			fv := args[0].Field(1) // Strings field of the `in` struct
			got := make([]string, 0, fv.Len())
			for j := 0; j < fv.Len(); j++ {
				got = append(got, fv.Index(j).String())
			}
			results[i] = got
		}(i)
	}

	// Wait until one resolution is inside the build, then let the
	// constructors finish.
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&calls) > 0
	}, 5*time.Second, time.Millisecond)
	close(release)
	wg.Wait()

	for i := range errs {
		require.NoError(t, errs[i])
	}
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls),
		"providers must run exactly once across concurrent resolutions of the same group")
	for _, got := range results {
		assert.ElementsMatch(t, []string{"a", "b", "c"}, got)
	}
}

// A failed shared build must release its waiters with the same failure and
// must not cache partial results, so the next resolution retries.
func TestGroupBuildConcurrentSharedFailure(t *testing.T) {
	c := New(setRand(rand.New(rand.NewSource(0))))
	s := c.scope

	type out struct {
		Out

		V string `group:"x"`
	}

	release := make(chan struct{})
	var calls int32
	require.NoError(t, s.Provide(func() out {
		atomic.AddInt32(&calls, 1)
		<-release
		return out{V: "a"}
	}))
	require.NoError(t, s.Provide(func() (out, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return out{}, errFake
	}))

	type in struct {
		In

		Strings []string `group:"x"`
	}
	pl, err := newParamList(reflect.TypeOf(func(in) {}), s)
	require.NoError(t, err)

	const numResolutions = 4
	errs := make([]error, numResolutions)
	var wg sync.WaitGroup
	for i := 0; i < numResolutions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := pl.BuildList(s)
			errs[i] = err
		}(i)
	}

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&calls) > 0
	}, 5*time.Second, time.Millisecond)
	close(release)
	wg.Wait()

	for i := range errs {
		require.Error(t, errs[i], "every sharer of the failed build must observe the failure")
	}
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls),
		"providers must run exactly once across concurrent resolutions")

	// The failed build cached nothing: the group is empty and a retry
	// runs the providers again.
	assert.Empty(t, s.getValueGroup("x", reflect.TypeOf("")))
	_, err = pl.BuildList(s)
	require.Error(t, err)
	assert.Equal(t, int32(4), atomic.LoadInt32(&calls),
		"failed build must be retried in full")
}

var errFake = errors.New("fake error")
