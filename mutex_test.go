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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReentrantMutex(t *testing.T) {
	t.Parallel()

	t.Run("same goroutine may lock repeatedly", func(t *testing.T) {
		t.Parallel()

		var m reentrantMutex
		m.Lock()
		m.Lock()
		m.Lock()

		done := make(chan struct{})
		go func() {
			defer close(done)
			m.Lock()
			m.Unlock()
		}()

		// The other goroutine is blocked until all nested locks are released.
		select {
		case <-done:
			t.Fatal("second goroutine acquired the mutex while it was still held")
		case <-time.After(10 * time.Millisecond):
		}

		m.Unlock()
		m.Unlock()
		select {
		case <-done:
			t.Fatal("second goroutine acquired the mutex while it was still held")
		case <-time.After(10 * time.Millisecond):
		}

		m.Unlock()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("second goroutine did not acquire the mutex after it was released")
		}
	})

	t.Run("mutual exclusion across goroutines", func(t *testing.T) {
		t.Parallel()

		var (
			m       reentrantMutex
			counter int
			wg      sync.WaitGroup
		)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 100; j++ {
					m.Lock()
					m.Lock()
					counter++
					m.Unlock()
					m.Unlock()
				}
			}()
		}
		wg.Wait()
		assert.Equal(t, 800, counter)
	})

	t.Run("goid is stable and non-zero", func(t *testing.T) {
		t.Parallel()

		id := goid()
		require.NotZero(t, id)
		assert.Equal(t, id, goid())

		other := make(chan int64, 1)
		go func() { other <- goid() }()
		assert.NotEqual(t, id, <-other)
	})
}
