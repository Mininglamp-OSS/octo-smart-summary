package agent

import (
	"fmt"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// messageCache is an in-memory cache for fetched messages with owner isolation.
// Handles are bound to uid; Retrieve validates ownership before returning data.
var messageCache = newMessageCache()

type cacheEntry struct {
	messages  []pipeline.Message
	uid       string
	createdAt time.Time
}

type msgCache struct {
	mu      sync.RWMutex
	store   map[string]cacheEntry
	counter int
	maxSize int           // max number of entries before eviction
	ttl     time.Duration // time-to-live for entries
}

func newMessageCache() *msgCache {
	return &msgCache{
		store:   make(map[string]cacheEntry),
		maxSize: 1000, // default capacity limit
		ttl:     30 * time.Minute,
	}
}

// Store saves messages bound to a uid and returns a unique handle.
// The handle encodes the uid for ownership validation on Retrieve.
func (c *msgCache) Store(messages []pipeline.Message, uid string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict expired entries
	c.evictExpired()

	// Evict oldest if at capacity
	if len(c.store) >= c.maxSize {
		c.evictOldest()
	}

	c.counter++
	handle := fmt.Sprintf("msg_%s_%d", safeHandleUID(uid), c.counter)
	c.store[handle] = cacheEntry{
		messages:  messages,
		uid:       uid,
		createdAt: time.Now(),
	}
	return handle
}

// Retrieve fetches messages by handle, validating that the requesting uid matches the owner.
// Returns nil if handle not found or uid mismatch.
func (c *msgCache) Retrieve(handle, uid string) []pipeline.Message {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.store[handle]
	if !ok {
		return nil
	}

	// Ownership validation
	if entry.uid != uid {
		return nil
	}

	// TTL check
	if time.Since(entry.createdAt) > c.ttl {
		return nil
	}

	return entry.messages
}

// evictExpired removes entries older than TTL. Must be called with lock held.
func (c *msgCache) evictExpired() {
	now := time.Now()
	for k, v := range c.store {
		if now.Sub(v.createdAt) > c.ttl {
			delete(c.store, k)
		}
	}
}

// evictOldest removes the oldest entry by createdAt. Must be called with lock held.
func (c *msgCache) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, v := range c.store {
		if first || v.createdAt.Before(oldestTime) {
			oldestKey = k
			oldestTime = v.createdAt
			first = false
		}
	}
	if oldestKey != "" {
		delete(c.store, oldestKey)
	}
}

// safeHandleUID creates a URL-safe short representation of uid for handle prefix.
// Uses first 8 chars of uid (or full uid if shorter).
func safeHandleUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

// GetMessageCache returns the package-level message cache instance.
// Exposed for out-of-package callers (e.g., the agent summary handler)
// that need to retrieve cached messages by handle when building citations.
func GetMessageCache() *msgCache {
	return messageCache
}

// ResetForTest empties the global message cache. Test-only helper —
// handlers/tests that assert on cache miss must call this in setup
// to isolate from sibling tests that populate the cache (necessary
// because -shuffle=on can interleave test order).
//
// Not exported via a _test.go file because it must be callable from
// sibling packages (e.g. internal/api/handler).
func ResetForTest() {
	messageCache.mu.Lock()
	defer messageCache.mu.Unlock()
	messageCache.store = make(map[string]cacheEntry)
	messageCache.counter = 0
}
