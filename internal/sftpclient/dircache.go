package sftpclient

import "sync"

// dirCache remembers directories this session has already confirmed on the
// remote host. Without it, a batch of small uploads pays one Stat/Mkdir round
// trip per file just to make sure its parent exists.
type dirCache struct {
	mu   sync.RWMutex
	dirs map[string]struct{}
}

func newDirCache() *dirCache {
	return &dirCache{dirs: map[string]struct{}{}}
}

func (c *dirCache) has(dir string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.dirs[dir]
	return ok
}

func (c *dirCache) add(dir string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.dirs[dir] = struct{}{}
	c.mu.Unlock()
}

func (c *dirCache) remove(dir string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.dirs, dir)
	c.mu.Unlock()
}

// clear forgets every directory. Deletes and renames can invalidate an
// arbitrary part of the tree, so the cheap answer is to forget all of it.
func (c *dirCache) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.dirs = map[string]struct{}{}
	c.mu.Unlock()
}
