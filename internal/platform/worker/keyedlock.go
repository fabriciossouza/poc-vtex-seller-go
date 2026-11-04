package worker

import "sync"

// keyedLock serializes work per key and forgets a key once nobody holds or
// waits for it, so the map does not grow with the number of orders.
type keyedLock struct {
	mu      sync.Mutex
	entries map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedLock) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.entries == nil {
		k.entries = map[string]*lockEntry{}
	}
	e := k.entries[key]
	if e == nil {
		e = &lockEntry{}
		k.entries[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(k.entries, key)
		}
		k.mu.Unlock()
	}
}
