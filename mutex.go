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
	"bytes"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
)

// reentrantMutex is a mutual exclusion lock that may be locked multiple
// times by the same goroutine. It must be unlocked as many times as it was
// locked before another goroutine can acquire it.
//
// This is used to serialize graph mutations (Provide, Decorate) against
// resolutions (Invoke) across a scope tree while still permitting nested
// resolutions on a single goroutine, e.g. an Invoke issued from inside a
// function that is itself being invoked.
type reentrantMutex struct {
	mu sync.Mutex
	// owner is the goroutine ID currently holding the lock, or zero if the
	// lock is free. Goroutine IDs start at 1, so the zero value is free.
	owner atomic.Int64
	depth int // number of times the owner has locked the mutex
}

var _ sync.Locker = (*reentrantMutex)(nil)

// Lock locks m. If the calling goroutine already holds m, the lock count
// is incremented and Lock returns immediately.
func (m *reentrantMutex) Lock() {
	id := goid()
	// If owner == id, this goroutine is the one holding m.mu: owner is
	// only ever set to a goroutine's own ID by that goroutine itself, so
	// observing our own ID here means we hold m.mu and may touch depth
	// without further synchronization.
	if m.owner.Load() == id {
		m.depth++
		return
	}
	m.mu.Lock()
	m.owner.Store(id)
	m.depth = 1
}

// Unlock undoes a single call to Lock. The mutex is released once the
// lock count drops to zero.
func (m *reentrantMutex) Unlock() {
	m.depth--
	if m.depth == 0 {
		m.owner.Store(0)
		m.mu.Unlock()
	}
}

// goid returns the ID of the current goroutine.
func goid() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// The stack trace begins with "goroutine <id> [".
	line := buf[:n]
	line = bytes.TrimPrefix(line, []byte("goroutine "))
	line = line[:bytes.IndexByte(line, ' ')]
	id, err := strconv.ParseInt(string(line), 10, 64)
	if err != nil {
		panic(fmt.Sprintf("dig: cannot determine goroutine ID: %v", err))
	}
	return id
}
