package agent

import (
	"fmt"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/google/uuid"
)

// messageCache is an in-memory cache for fetched messages with owner and
// conversation-scope isolation. A handle is valid only for the exact
// (uid, sessionID) pair that minted it. summary_workspace supplies its derived
// Agent session id here (space + public session + scope version), while Legacy
// supplies its existing public session id.
var messageCache = newMessageCache()

type cacheEntry struct {
	messages  []pipeline.Message
	uid       string
	sessionID string
	createdAt time.Time
}

type msgCache struct {
	mu      sync.RWMutex
	store   map[string]cacheEntry
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

// Store saves messages bound to an exact uid/session identity and returns a
// unique opaque handle. Identity is stored in the entry rather than trusted
// from the handle text.
func (c *msgCache) Store(messages []pipeline.Message, uid, sessionID string) string {
	if uid == "" || sessionID == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict expired entries
	c.evictExpired()

	// Evict oldest if at capacity
	if len(c.store) >= c.maxSize {
		c.evictOldest()
	}

	handle := fmt.Sprintf("msg_%s_%s", safeHandleUID(uid), uuid.NewString())
	c.store[handle] = cacheEntry{
		messages:  messages,
		uid:       uid,
		sessionID: sessionID,
		createdAt: time.Now(),
	}
	return handle
}

// Retrieve fetches messages by handle, validating the exact owner and session
// identity. Returns nil for a missing, expired, or cross-session handle.
func (c *msgCache) Retrieve(handle, uid, sessionID string) []pipeline.Message {
	if handle == "" || uid == "" || sessionID == "" {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.store[handle]
	if !ok {
		return nil
	}

	// Ownership validation
	if entry.uid != uid || entry.sessionID != sessionID {
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
}
