package dublift

import (
	"container/list"
	"context"
	"errors"
	"strings"
	"sync"
)

type cacheOwnerKey struct{}

var errRangeNotCached = errors.New("file range has not been downloaded for playback")

type cacheEntry struct {
	key     string
	data    []byte
	rangeID string
	offset  int64
}
type cacheFlight struct {
	done   chan struct{}
	data   []byte
	err    error
	offset int64
	size   int64
}
type ByteCache struct {
	mu           sync.Mutex
	max, used    int64
	items        map[string]*list.Element
	lru          *list.List
	pending      map[string]*cacheFlight
	ranges       map[string][]*list.Element
	rangePending map[string][]*cacheFlight
	epoch        uint64
}

func NewByteCache(limit int64) *ByteCache {
	return &ByteCache{max: limit, items: map[string]*list.Element{}, lru: list.New(), pending: map[string]*cacheFlight{}, ranges: map[string][]*list.Element{}, rangePending: map[string][]*cacheFlight{}}
}
func (c *ByteCache) Resize(n int64) { c.mu.Lock(); defer c.mu.Unlock(); c.max = n; c.evict() }
func (c *ByteCache) evict() {
	for c.used > c.max && c.lru.Len() > 0 {
		e := c.lru.Back()
		v := e.Value.(cacheEntry)
		delete(c.items, v.key)
		if v.rangeID != "" {
			c.removeRange(v.rangeID, e)
		}
		c.used -= int64(len(v.data))
		c.lru.Remove(e)
	}
}
func (c *ByteCache) removeRange(id string, target *list.Element) {
	entries := c.ranges[id]
	for i, e := range entries {
		if e == target {
			c.ranges[id] = append(entries[:i], entries[i+1:]...)
			break
		}
	}
	if len(c.ranges[id]) == 0 {
		delete(c.ranges, id)
	}
}
func (c *ByteCache) ownerKey(ctx context.Context, key string) string {
	if owner, ok := ctx.Value(cacheOwnerKey{}).(string); ok {
		return owner + "|" + key
	}
	return key
}
func (c *ByteCache) Used() int64 { c.mu.Lock(); defer c.mu.Unlock(); return c.used }
func (c *ByteCache) Lookup(ctx context.Context, key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.items[c.ownerKey(ctx, key)]; e != nil {
		c.lru.MoveToFront(e)
		return e.Value.(cacheEntry).data, true
	}
	return nil, false
}

func (c *ByteCache) Put(ctx context.Context, key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil || int64(len(data)) > c.max {
		return
	}
	key = c.ownerKey(ctx, key)
	if old := c.items[key]; old != nil {
		c.used -= int64(len(old.Value.(cacheEntry).data))
		c.lru.Remove(old)
	}
	c.items[key] = c.lru.PushFront(cacheEntry{key: key, data: data})
	c.used += int64(len(data))
	c.evict()
}
func (c *ByteCache) Get(ctx context.Context, key string, build func() ([]byte, error)) ([]byte, error) {
	originalKey := key
	key = c.ownerKey(ctx, key)
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
		c.items[key] = c.lru.PushFront(cacheEntry{key: key, data: b})
		c.used += int64(len(b))
		c.evict()
	}
	if c.pending[key] == f {
		delete(c.pending, key)
	}
	close(f.done)
	return b, e
}

// ReadRange shares overlapping finite file reads, including reads whose
// requested offsets differ. Only gaps are fetched from the origin.
func (c *ByteCache) ReadRange(ctx context.Context, id string, off, size int64, build func(int64, int64) ([]byte, error)) ([]byte, error) {
	if size <= 0 {
		return nil, nil
	}
	id = c.ownerKey(ctx, "range:"+id)
	out := make([]byte, size)
	end := off + size
	for cursor := off; cursor < end; {
		if err := ctx.Err(); err != nil {
			return out[:cursor-off], err
		}
		c.mu.Lock()
		epoch := c.epoch
		var hit *list.Element
		next := end
		for _, e := range c.ranges[id] {
			v := e.Value.(cacheEntry)
			if v.offset <= cursor && cursor < v.offset+int64(len(v.data)) {
				hit = e
				break
			}
			if v.offset > cursor && v.offset < next {
				next = v.offset
			}
		}
		if hit != nil {
			v := hit.Value.(cacheEntry)
			n := min(end-cursor, v.offset+int64(len(v.data))-cursor)
			copy(out[cursor-off:cursor-off+n], v.data[cursor-v.offset:cursor-v.offset+n])
			c.lru.MoveToFront(hit)
			c.mu.Unlock()
			cursor += n
			continue
		}
		var pending *cacheFlight
		for _, f := range c.rangePending[id] {
			if f.offset <= cursor && cursor < f.offset+f.size {
				pending = f
				break
			}
			if f.offset > cursor && f.offset < next {
				next = f.offset
			}
		}
		if pending != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return out[:cursor-off], ctx.Err()
			case <-pending.done:
				if pending.err != nil && !errors.Is(pending.err, context.Canceled) && !errors.Is(pending.err, context.DeadlineExceeded) {
					return out[:cursor-off], pending.err
				}
				if pending.err == nil {
					n := min(end-cursor, pending.offset+int64(len(pending.data))-cursor)
					if n > 0 {
						copy(out[cursor-off:cursor-off+n], pending.data[cursor-pending.offset:cursor-pending.offset+n])
						cursor += n
					}
				}
			}
			continue
		}
		if build == nil {
			c.mu.Unlock()
			return out[:cursor-off], errRangeNotCached
		}
		n := next - cursor
		f := &cacheFlight{done: make(chan struct{}), offset: cursor, size: n}
		c.rangePending[id] = append(c.rangePending[id], f)
		c.mu.Unlock()
		data, err := build(cursor, n)
		if err == nil && int64(len(data)) != n {
			err = errors.New("short ranged file read")
		}
		c.mu.Lock()
		f.data, f.err = data, err
		flights := c.rangePending[id]
		for i, p := range flights {
			if p == f {
				c.rangePending[id] = append(flights[:i], flights[i+1:]...)
				break
			}
		}
		if len(c.rangePending[id]) == 0 {
			delete(c.rangePending, id)
		}
		if err == nil && ctx.Err() == nil && epoch == c.epoch && int64(len(data)) <= c.max {
			entry := c.lru.PushFront(cacheEntry{data: data, rangeID: id, offset: cursor})
			c.ranges[id] = append(c.ranges[id], entry)
			c.used += int64(len(data))
			c.evict()
		}
		close(f.done)
		c.mu.Unlock()
		if err != nil {
			return out[:cursor-off], err
		}
		copy(out[cursor-off:cursor-off+n], data)
		cursor += n
	}
	return out, nil
}

func (c *ByteCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.items = map[string]*list.Element{}
	c.pending = map[string]*cacheFlight{}
	c.ranges = map[string][]*list.Element{}
	c.rangePending = map[string][]*cacheFlight{}
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
	for key, entries := range c.ranges {
		if strings.HasPrefix(key, owner+"|") {
			for _, e := range entries {
				c.used -= int64(len(e.Value.(cacheEntry).data))
				c.lru.Remove(e)
			}
			delete(c.ranges, key)
		}
	}
	for key := range c.rangePending {
		if strings.HasPrefix(key, owner+"|") {
			delete(c.rangePending, key)
		}
	}
	for key := range c.pending {
		if strings.HasPrefix(key, owner+"|") {
			delete(c.pending, key)
		}
	}
}
