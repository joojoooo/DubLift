package dublift

import (
	"container/list"
	"context"
	"errors"
	"strings"
	"sync"
)

type cacheOwnerKey struct{}

type cacheEntry struct {
	key  string
	data []byte
}
type cacheFlight struct {
	done chan struct{}
	data []byte
	err  error
}
type ByteCache struct {
	mu        sync.Mutex
	max, used int64
	items     map[string]*list.Element
	lru       *list.List
	pending   map[string]*cacheFlight
	epoch     uint64
}

func NewByteCache(limit int64) *ByteCache {
	return &ByteCache{max: limit, items: map[string]*list.Element{}, lru: list.New(), pending: map[string]*cacheFlight{}}
}
func (c *ByteCache) Resize(n int64) { c.mu.Lock(); defer c.mu.Unlock(); c.max = n; c.evict() }
func (c *ByteCache) evict() {
	for c.used > c.max && c.lru.Len() > 0 {
		e := c.lru.Back()
		v := e.Value.(cacheEntry)
		delete(c.items, v.key)
		c.used -= int64(len(v.data))
		c.lru.Remove(e)
	}
}
func (c *ByteCache) Used() int64 { c.mu.Lock(); defer c.mu.Unlock(); return c.used }
func (c *ByteCache) Get(ctx context.Context, key string, build func() ([]byte, error)) ([]byte, error) {
	originalKey := key
	if owner, ok := ctx.Value(cacheOwnerKey{}).(string); ok {
		key = owner + "|" + key
	}
	c.mu.Lock()
	epoch := c.epoch
	if e := c.items[key]; e != nil {
		c.lru.MoveToFront(e)
		b := e.Value.(cacheEntry).data
		c.mu.Unlock()
		return b, nil
	}
	if f := c.pending[key]; f != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.done:
			if ctx.Err() == nil && (errors.Is(f.err, context.Canceled) || errors.Is(f.err, context.DeadlineExceeded)) {
				return c.Get(ctx, originalKey, build)
			}
			return f.data, f.err
		}
	}
	f := &cacheFlight{done: make(chan struct{})}
	c.pending[key] = f
	c.mu.Unlock()
	b, e := build()
	c.mu.Lock()
	defer c.mu.Unlock()
	f.data = b
	f.err = e
	if e == nil && ctx.Err() == nil && epoch == c.epoch && int64(len(b)) <= c.max {
		c.items[key] = c.lru.PushFront(cacheEntry{key, b})
		c.used += int64(len(b))
		c.evict()
	}
	if c.pending[key] == f {
		delete(c.pending, key)
	}
	close(f.done)
	return b, e
}

func (c *ByteCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.items = map[string]*list.Element{}
	c.pending = map[string]*cacheFlight{}
	c.lru.Init()
	c.used = 0
}
func (c *ByteCache) DeleteOwner(owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.items {
		if strings.HasPrefix(key, owner+"|") {
			c.used -= int64(len(e.Value.(cacheEntry).data))
			c.lru.Remove(e)
			delete(c.items, key)
		}
	}
	for key := range c.pending {
		if strings.HasPrefix(key, owner+"|") {
			delete(c.pending, key)
		}
	}
}
