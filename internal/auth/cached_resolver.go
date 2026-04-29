package auth

import (
	"context"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
)

type cacheEntry struct {
	uid      string
	expireAt time.Time
}

type CachedResolver struct {
	inner   middleware.TokenResolver
	cache   sync.Map
	ttl     time.Duration
	maxSize int
}

func NewCachedResolver(inner middleware.TokenResolver, ttl time.Duration, maxSize int) *CachedResolver {
	r := &CachedResolver{
		inner:   inner,
		ttl:     ttl,
		maxSize: maxSize,
	}
	go r.evictLoop()
	return r
}

func (r *CachedResolver) ResolveUID(ctx context.Context, token string) (string, error) {
	if cached, ok := r.cache.Load(token); ok {
		entry := cached.(*cacheEntry)
		if time.Now().Before(entry.expireAt) {
			return entry.uid, nil
		}
		r.cache.Delete(token)
	}
	uid, err := r.inner.ResolveUID(ctx, token)
	if err == nil && uid != "" {
		r.cache.Store(token, &cacheEntry{uid: uid, expireAt: time.Now().Add(r.ttl)})
	}
	return uid, err
}

func (r *CachedResolver) evictLoop() {
	ticker := time.NewTicker(r.ttl)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		count := 0
		r.cache.Range(func(key, value any) bool {
			count++
			entry := value.(*cacheEntry)
			if now.After(entry.expireAt) {
				r.cache.Delete(key)
			}
			return true
		})
		if count > r.maxSize {
			r.cache.Range(func(key, _ any) bool {
				r.cache.Delete(key)
				count--
				return count > r.maxSize
			})
		}
	}
}
