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
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/dig"
	"go.uber.org/dig/internal/digtest"
)

// Tests for transactional value group builds: a single resolution freezes
// the providers of a group and their order, delivers the group only if
// every element succeeds, and caches no partial results on failure.
func TestGroupBuildTransaction(t *testing.T) {
	t.Run("failure makes the build invisible and the next invoke retries", func(t *testing.T) {
		c := digtest.New(t, dig.SetRand(rand.New(rand.NewSource(0))))

		type out struct {
			dig.Out

			Result string `group:"x"`
		}

		var aCalls, bCalls, cCalls int
		c.RequireProvide(func() out {
			aCalls++
			return out{Result: "a"}
		})
		failB := true
		gaveErr := errors.New("great sadness")
		c.RequireProvide(func() (out, error) {
			bCalls++
			if failB {
				return out{}, gaveErr
			}
			return out{Result: "b"}, nil
		})
		c.RequireProvide(func() out {
			cCalls++
			return out{Result: "c"}
		})

		type in struct {
			dig.In

			Strings []string `group:"x"`
		}

		// Providers run in registration order, so the failing
		// constructor aborts the build before "c" runs. Nothing from
		// the failed builds may be cached: "a" must run again on the
		// next attempt instead of leaking its value into it.
		require.Error(t, c.Invoke(func(in) {}))
		require.Error(t, c.Invoke(func(in) {}))
		assert.Equal(t, 2, aCalls, "rolled-back constructors must run again on the next invoke")
		assert.Equal(t, 2, bCalls)
		assert.Equal(t, 0, cCalls)

		// Once the failure is fixed, the whole group is rebuilt and
		// delivered in one piece.
		failB = false
		var got []string
		require.NoError(t, c.Invoke(func(i in) { got = i.Strings }))
		assert.Equal(t, 3, aCalls)
		assert.Equal(t, 3, bCalls)
		assert.Equal(t, 1, cCalls)
		assert.ElementsMatch(t, []string{"a", "b", "c"}, got)
	})

	t.Run("group and regular parameters resolve against the same graph version", func(t *testing.T) {
		c := digtest.New(t, dig.SetRand(rand.New(rand.NewSource(0))))

		type outA struct {
			dig.Out

			Name string
			ID   int `group:"ids"`
		}
		var aCalls int
		c.RequireProvide(func() outA {
			aCalls++
			return outA{Name: "a", ID: 1}
		})

		type outB struct {
			dig.Out

			ID int `group:"ids"`
		}
		var bCalls int
		var bSawName string
		c.RequireProvide(func(name string) outB {
			bCalls++
			bSawName = name
			return outB{ID: 2}
		})

		type in struct {
			dig.In

			// The group is listed first so that the group build is
			// what triggers the shared constructor: the regular
			// parameter must observe the value it produced within the
			// same resolution.
			IDs  []int `group:"ids"`
			Name string
		}

		var gotName string
		var gotIDs []int
		require.NoError(t, c.Invoke(func(i in) {
			gotName = i.Name
			gotIDs = i.IDs
		}))
		assert.Equal(t, "a", gotName)
		assert.Equal(t, "a", bSawName,
			"group provider must see the value produced by an earlier provider in the same build")
		assert.ElementsMatch(t, []int{1, 2}, gotIDs)
		assert.Equal(t, 1, aCalls, "constructor shared by a regular and a grouped parameter must run once")
		assert.Equal(t, 1, bCalls)
	})

	t.Run("nested groups commit and roll back together", func(t *testing.T) {
		c := digtest.New(t, dig.SetRand(rand.New(rand.NewSource(0))))

		type innerOut struct {
			dig.Out

			V string `group:"inner"`
		}
		var innerCalls int
		c.RequireProvide(func() innerOut {
			innerCalls++
			return innerOut{V: "inner"}
		})

		type innerIn struct {
			dig.In

			Inner []string `group:"inner"`
		}
		type outerOut struct {
			dig.Out

			V string `group:"outer"`
		}
		var outerCalls int
		failOuter := true
		c.RequireProvide(func(i innerIn) (outerOut, error) {
			outerCalls++
			if failOuter {
				return outerOut{}, errors.New("outer sadness")
			}
			return outerOut{V: "outer"}, nil
		})

		type in struct {
			dig.In

			Outer []string `group:"outer"`
		}

		require.Error(t, c.Invoke(func(in) {}))
		assert.Equal(t, 1, innerCalls)
		assert.Equal(t, 1, outerCalls)

		failOuter = false
		var got []string
		require.NoError(t, c.Invoke(func(i in) { got = i.Outer }))
		assert.Equal(t, []string{"outer"}, got)
		assert.Equal(t, 2, innerCalls,
			"inner group staged by the failed transaction must be rebuilt")
		assert.Equal(t, 2, outerCalls)
	})

	t.Run("soft groups are unaffected", func(t *testing.T) {
		c := digtest.New(t, dig.SetRand(rand.New(rand.NewSource(0))))

		type out struct {
			dig.Out

			V string `group:"s"`
		}
		var calls int
		c.RequireProvide(func() out {
			calls++
			return out{V: "v"}
		})

		type in struct {
			dig.In

			Strings []string `group:"s,soft"`
		}
		require.NoError(t, c.Invoke(func(i in) {
			assert.Empty(t, i.Strings)
		}))
		assert.Equal(t, 0, calls, "soft group providers must not be called by the group build")
	})
}
