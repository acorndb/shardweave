package server

import "sync"

// connIDAllocator issues frontend connection ids the way MySQL does
// (WL#7293): the counter starts at 1, a 32-bit wrap skips 0, and ids held by
// live sessions are never re-issued. The live set is empty at process start,
// so ids are only reused after a wrap (2^32 allocations).
type connIDAllocator struct {
	mu   sync.Mutex
	next uint32
	live map[uint32]struct{}
}

// alloc reserves and returns the next free connection id.
func (a *connIDAllocator) alloc() uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.live == nil {
		a.live = make(map[uint32]struct{})
	}
	for {
		a.next++
		if a.next == 0 { // 32-bit wrap: MySQL restarts from 1, never 0
			a.next = 1
		}
		if _, used := a.live[a.next]; used {
			continue
		}
		a.live[a.next] = struct{}{}
		return a.next
	}
}

// free releases id once its frontend session is gone.
func (a *connIDAllocator) free(id uint32) {
	if id == 0 {
		return
	}
	a.mu.Lock()
	delete(a.live, id)
	a.mu.Unlock()
}
