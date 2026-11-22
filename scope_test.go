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

package dig_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/dig"
	"go.uber.org/dig/internal/digtest"
)

func TestScopedOperations(t *testing.T) {
	t.Parallel()

	t.Run("private provides", func(t *testing.T) {
		c := digtest.New(t)
		s := c.Scope("child")
		type A struct{}

		f := func(a *A) {
			assert.NotEqual(t, nil, a)
		}

		s.RequireProvide(func() *A { return &A{} })
		s.RequireInvoke(f)
		assert.Error(t, c.Invoke(f))
	})

	t.Run("private provides inherits", func(t *testing.T) {
		type A struct{}
		type B struct{}

		useA := func(a *A) {
			assert.NotEqual(t, nil, a)
		}
		useB := func(b *B) {
			assert.NotEqual(t, nil, b)
		}

		c := digtest.New(t)
		c.RequireProvide(func() *A { return &A{} })

		child := c.Scope("child")
		child.RequireProvide(func() *B { return &B{} })
		child.RequireInvoke(useA)
		child.RequireInvoke(useB)

		grandchild := child.Scope("grandchild")

		grandchild.RequireInvoke(useA)
		grandchild.RequireInvoke(useB)
		assert.Error(t, c.Invoke(useB))
	})

	t.Run("private provides doesn't depend on invoke order", func(t *testing.T) {
		c := digtest.New(t)
		child := c.Scope("child")

		c.RequireProvide(func() string { return "container" })
		child.RequireProvide(func() string { return "child" })

		verifyContainerStr := func(s string) {
			assert.Equal(t, "container", s)
		}
		verifyChildStr := func(s string) {
			assert.Equal(t, "child", s)
		}

		c.RequireInvoke(verifyContainerStr)
		child.RequireInvoke(verifyChildStr)
	})

	t.Run("provides to top-level Container propagates to all scopes", func(t *testing.T) {
		type A struct{}

		// Scope tree:
		//     root  <-- Provide(func() *A)
		//    /    \
		//   c1	    c2
		//   |     /  \
		//   gc1  gc2  gc3
		var allScopes []*digtest.Scope
		root := digtest.New(t)

		allScopes = append(allScopes, root.Scope("child 1"), root.Scope("child 2"))
		allScopes = append(allScopes, allScopes[0].Scope("grandchild 1"), allScopes[1].Scope("grandchild 2"), allScopes[1].Scope("grandchild 3"))

		root.RequireProvide(func() *A {
			return &A{}
		})

		// top-level provide should be available in all the scopes.
		for _, scope := range allScopes {
			scope.RequireInvoke(func(a *A) {})
		}
	})

	t.Run("provide with Export", func(t *testing.T) {
		// Scope tree:
		//     root
		//    /    \
		//   c1	    c2
		//   |     /  \
		//   gc1  gc2  gc3 <-- Provide(func() *A)

		root := digtest.New(t)
		var allScopes []*digtest.Scope

		allScopes = append(allScopes, root.Scope("child 1"), root.Scope("child 2"))
		allScopes = append(allScopes, allScopes[0].Scope("grandchild 1"), allScopes[1].Scope("grandchild 2"), allScopes[1].Scope("grandchild 3"))

		type A struct{}
		// provide to the leaf Scope with Export option set.
		allScopes[len(allScopes)-1].RequireProvide(func() *A {
			return &A{}
		}, dig.Export(true))

		// since constructor was provided with Export option, this should let all the Scopes below should see it.
		for _, scope := range allScopes {
			scope.RequireInvoke(func(a *A) {})
		}
	})

	t.Run("parent shares values with children", func(t *testing.T) {
		type (
			T1 struct{ s string }
			T2 struct{}
		)

		parent := digtest.New(t)

		parent.RequireProvide(func() T1 {
			assert.Fail(t, "parent should not be called")
			return T1{"parent"}
		})

		child := parent.Scope("child")

		var childCalled bool
		defer func() {
			assert.True(t, childCalled, "child constructor must be called")
		}()
		child.RequireProvide(func() T1 {
			childCalled = true
			return T1{"child"}
		})

		child.RequireProvide(func(v T1) T2 {
			assert.Equal(t, "child", v.s,
				"value should be built by child")
			return T2{}
		})

		child.RequireInvoke(func(T2) {})
	})
}

func TestScopeFailures(t *testing.T) {
	t.Parallel()

	t.Run("introduce a cycle with child", func(t *testing.T) {
		// what root sees:
		// A <- B    C
		// |         ^
		// |_________|
		//
		// what child sees:
		// A <- B <- C
		// |         ^
		// |_________|
		type A struct{}
		type B struct{}
		type C struct{}
		newA := func(*C) *A { return &A{} }
		newB := func(*A) *B { return &B{} }
		newC := func(*B) *C { return &C{} }

		// Create a child Scope, and introduce a cycle
		// in the child only.
		check := func(c *digtest.Container, fails bool) {
			s := c.Scope("child")
			c.RequireProvide(newA)
			s.RequireProvide(newB)
			err := c.Provide(newC)

			if fails {
				assert.Error(t, err, "expected a cycle to be introduced in the child")
				assert.Contains(t, err.Error(), `[scope "child"]`)
			} else {
				assert.NoError(t, err)
			}
		}

		// Same as check, but this time child should inherit
		// parent-provided constructors upon construction.
		checkWithInheritance := func(c *digtest.Container, fails bool) {
			c.RequireProvide(newA)
			s := c.Scope("child")
			s.RequireProvide(newB)
			err := c.Provide(newC)
			if fails {
				assert.Error(t, err, "expected a cycle to be introduced in the child")
				assert.Contains(t, err.Error(), `[scope "child"]`)
			} else {
				assert.NoError(t, err)
			}
		}

		// Test using different permutations
		nodeferContainers := []func() *digtest.Container{
			func() *digtest.Container { return digtest.New(t) },
			func() *digtest.Container { return digtest.New(t, dig.DryRun(true)) },
			func() *digtest.Container { return digtest.New(t, dig.DryRun(false)) },
		}
		// Container permutations with DeferAcyclicVerification.
		deferredContainers := []func() *digtest.Container{
			func() *digtest.Container { return digtest.New(t, dig.DeferAcyclicVerification()) },
			func() *digtest.Container { return digtest.New(t, dig.DeferAcyclicVerification(), dig.DryRun(true)) },
			func() *digtest.Container { return digtest.New(t, dig.DeferAcyclicVerification(), dig.DryRun(false)) },
		}

		for _, c := range nodeferContainers {
			check(c(), true)
			checkWithInheritance(c(), true)
		}

		// with deferAcyclicVerification, these should not
		// error on Provides.
		for _, c := range deferredContainers {
			check(c(), false)
			checkWithInheritance(c(), false)
		}
	})

	t.Run("introduce a cycle with Export option", func(t *testing.T) {
		// what root and child1 sees:
		// A <- B    C
		// |         ^
		// |_________|
		//
		// what child2 sees:
		// A <- B <- C
		// |         ^
		// |_________|

		type A struct{}
		type B struct{}
		type C struct{}
		newA := func(*C) *A { return &A{} }
		newB := func(*A) *B { return &B{} }
		newC := func(*B) *C { return &C{} }

		root := digtest.New(t)
		child1 := root.Scope("child 1")
		child2 := root.Scope("child 2")

		// A <- B made available to all Scopes with root provision.
		root.RequireProvide(newA)

		// B <- C made available to only child 2 with private provide.
		child2.RequireProvide(newB)

		// C <- A made available to all Scopes with Export provide.
		err := child1.Provide(newC, dig.Export(true))
		assert.Error(t, err, "expected a cycle to be introduced in child 2")
		assert.Contains(t, err.Error(), `[scope "child 2"]`)
	})

	t.Run("private provides do not propagate upstream", func(t *testing.T) {
		type A struct{}

		root := digtest.New(t)
		c := root.Scope("child")
		gc := c.Scope("grandchild")
		gc.RequireProvide(func() *A { return &A{} })

		assert.Error(t, root.Invoke(func(a *A) {}), "invoking on grandchild's private-provided type should fail")
		assert.Error(t, c.Invoke(func(a *A) {}), "invoking on child's private-provided type should fail")
	})

	t.Run("private provides to child should be available to grandchildren, but not root", func(t *testing.T) {
		type A struct{}
		// Scope tree:
		//     root
		//      |
		//     child  <-- Provide(func() *A)
		//     /  \
		//   gc1   gc2
		root := digtest.New(t)
		c := root.Scope("child")
		gc := c.Scope("grandchild")

		c.RequireProvide(func() *A { return &A{} })

		err := root.Invoke(func(a *A) {})
		assert.Error(t, err, "expected Invoke in root container on child's private-provided type to fail")
		assert.Contains(t, err.Error(), "missing type: *dig_test.A")

		gc.RequireInvoke(func(a *A) {})
	})
}

func TestScopeValueGroups(t *testing.T) {
	t.Run("provide in parent and child", func(t *testing.T) {
		type result struct {
			dig.Out

			Value string `group:"foo"`
		}

		root := digtest.New(t)
		root.RequireProvide(func() result {
			return result{Value: "a"}
		})

		root.RequireProvide(func() result {
			return result{Value: "b"}
		})

		root.RequireProvide(func() result {
			return result{Value: "c"}
		})

		child := root.Scope("child")
		child.RequireProvide(func() result {
			return result{Value: "d"}
		})

		type param struct {
			dig.In

			Values []string `group:"foo"`
		}

		t.Run("invoke parent", func(t *testing.T) {
			root.RequireInvoke(func(i param) {
				assert.ElementsMatch(t, []string{"a", "b", "c"}, i.Values)
			})
		})

		t.Run("invoke child", func(t *testing.T) {
			child.RequireInvoke(func(i param) {
				assert.ElementsMatch(t, []string{"a", "b", "c", "d"}, i.Values)
			})
		})
	})

	t.Run("value group as a parent dependency", func(t *testing.T) {
		// Tree:
		//
		//   root      defines a function that consumes the value group
		//    |
		//    |
		//   child     produces values to the value group

		type T1 struct{}
		type param struct {
			dig.In

			Values []string `group:"foo"`
		}

		root := digtest.New(t)

		root.RequireProvide(func(p param) T1 {
			assert.ElementsMatch(t, []string{"a", "b", "c"}, p.Values)
			return T1{}
		})

		child := root.Scope("child")
		child.RequireProvide(func() string { return "a" }, dig.Group("foo"), dig.Export(true))
		child.RequireProvide(func() string { return "b" }, dig.Group("foo"), dig.Export(true))
		child.RequireProvide(func() string { return "c" }, dig.Group("foo"), dig.Export(true))

		// Invocation in child should see values provided to the child,
		// even though the constructor we're invoking is provided in
		// the parent.
		child.RequireInvoke(func(T1) {})
	})
}

// This tests that a child scope correctly copies its parent's graph,
// including information about the order of each node.
// Otherwise, during cycle detection, constructor nodes might
// return 0 as the order for all functions in the root scope,
// causing cycle detection to detect cycles that don't exist.
func TestFalsePositiveScopeCycleDetection(t *testing.T) {
	t.Run("single provide", func(t *testing.T) {
		root := digtest.New(t)
		root.RequireProvide(func(val string) int { return 0 })
		root.RequireProvide(func() string { return "sample" })
		child := root.Scope("child")
		// Cycle detection would error here because previous two provides
		// would both have order 0 for child scope.
		child.RequireProvide(func() float32 { return 0 })
	})
}

// These tests verify the ownership rules of scoped value resolution:
// values constructed while resolving a scope are cached in that scope,
// are reused only within it, and never leak into its parent or siblings.
func TestScopeValueCaching(t *testing.T) {
	t.Parallel()

	t.Run("values constructed in a child are cached in the child, not the parent", func(t *testing.T) {
		type T struct{ n int }

		c := digtest.New(t)

		var calls int
		c.RequireProvide(func() *T {
			calls++
			return &T{n: calls}
		})

		child1 := c.Scope("child 1")
		child2 := c.Scope("child 2")

		// The same scope reuses the instance it constructed.
		var t1a, t1b *T
		require.NoError(t, child1.Invoke(func(t *T) { t1a = t }))
		require.NoError(t, child1.Invoke(func(t *T) { t1b = t }))
		assert.Same(t, t1a, t1b, "same scope must reuse its cached instance")
		assert.Equal(t, 1, calls, "constructor must be called at most once per scope")

		// A sibling scope gets its own instance, not child1's.
		var t2 *T
		require.NoError(t, child2.Invoke(func(t *T) { t2 = t }))
		assert.NotSame(t, t1a, t2, "sibling scopes must not share instances")
		assert.Equal(t, 2, calls)

		// The children's resolutions did not populate the parent's cache:
		// the parent constructs its own singleton.
		var rootT *T
		require.NoError(t, c.Invoke(func(t *T) { rootT = t }))
		assert.NotSame(t, t1a, rootT, "parent must not reuse a child's instance")
		assert.NotSame(t, t2, rootT, "parent must not reuse a child's instance")
		assert.Equal(t, 3, calls)

		// A scope created after the parent resolved inherits the parent's
		// singleton.
		child3 := c.Scope("child 3")
		var t3 *T
		require.NoError(t, child3.Invoke(func(t *T) { t3 = t }))
		assert.Same(t, rootT, t3, "child must reuse the instance cached by its parent")
		assert.Equal(t, 3, calls)
	})

	t.Run("failed resolution in a child does not pollute the parent cache", func(t *testing.T) {
		type Dep struct{ n int }
		type T struct{}

		c := digtest.New(t)
		c.RequireProvide(func() *Dep { return &Dep{n: 1} })
		c.RequireProvide(func(*Dep) (*T, error) { return nil, errors.New("great sadness") })

		child := c.Scope("child")
		require.Error(t, child.Invoke(func(*T) {}),
			"resolution of a transitive dependency of a failing constructor must fail")

		// Although constructing *T failed, *Dep was successfully built
		// during the child's resolution. It must have been cached in the
		// child, not committed to the parent.
		var childDep, rootDep *Dep
		require.NoError(t, child.Invoke(func(d *Dep) { childDep = d }))
		require.NoError(t, c.Invoke(func(d *Dep) { rootDep = d }))
		assert.NotSame(t, childDep, rootDep,
			"parent cache must not contain values constructed by the child")
	})

	t.Run("group values constructed in a child are owned by the child", func(t *testing.T) {
		type param struct {
			dig.In
			Values []string `group:"foo"`
		}

		c := digtest.New(t)

		var calls int
		c.RequireProvide(func() string {
			calls++
			return "a"
		}, dig.Group("foo"))

		child := c.Scope("child")

		require.NoError(t, child.Invoke(func(p param) {
			assert.Equal(t, []string{"a"}, p.Values)
		}))
		assert.Equal(t, 1, calls)

		// The parent did not receive the group value constructed for the
		// child; it constructs its own.
		require.NoError(t, c.Invoke(func(p param) {
			assert.Equal(t, []string{"a"}, p.Values)
		}))
		assert.Equal(t, 2, calls)
	})

	t.Run("callbacks observe the resolving scope's view", func(t *testing.T) {
		type T struct{}

		c := digtest.New(t)

		var beforeCalled, called bool
		c.RequireProvide(func() *T { return &T{} },
			dig.WithProviderBeforeCallback(func(dig.BeforeCallbackInfo) { beforeCalled = true }),
			dig.WithProviderCallback(func(info dig.CallbackInfo) {
				called = true
				assert.NoError(t, info.Error)
			}),
		)

		child := c.Scope("child")
		require.NoError(t, child.Invoke(func(*T) {}))
		assert.True(t, beforeCalled, "before callback must run for the child's resolution")
		assert.True(t, called, "callback must run for the child's resolution")
	})

	t.Run("recreated scopes build independent instances", func(t *testing.T) {
		type T struct{ n int }

		c := digtest.New(t)
		var calls int
		c.RequireProvide(func() *T {
			calls++
			return &T{n: calls}
		})

		// Simulate request-scoped usage: each scope is used for one
		// "request" and discarded afterwards.
		var instances []*T
		for i := 0; i < 3; i++ {
			scope := c.Scope("request")
			require.NoError(t, scope.Invoke(func(t *T) {
				instances = append(instances, t)
			}))
		}
		for i := range instances {
			for j := range instances {
				if i != j {
					assert.NotSame(t, instances[i], instances[j],
						"each scope must construct its own independent instance")
				}
			}
		}
	})
}
