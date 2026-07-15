// Package timezone centralizes the project's wall-clock reference so database,
// backend logic and frontend agree on one zone. Default is Asia/Shanghai to
// preserve historical behavior; set TZ at process start to override.
//
// Background: the scheduler previously mixed time.Now().UTC() with a DSN using
// loc=Local (container TZ=Asia/Shanghai), causing an 8-hour skew between what
// the frontend selected (e.g. 17:00 Beijing) and when tasks actually fired.
// All "current time" reads must go through Now() so writes, reads, comparisons
// and schedule math share the same zone.
package timezone

import (
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// defaultZone is used when TZ env is empty; kept private so callers can't bake
// the literal into unrelated code paths.
const defaultZone = "Asia/Shanghai"

var (
	loc     *time.Location
	locOnce sync.Once
)

// Location returns the process timezone, resolved once at first call:
//   - $TZ if set and loadable
//   - otherwise Asia/Shanghai
//   - if neither loads (missing zoneinfo / bad TZ value) fall back to a fixed
//     UTC+8 offset and warn, so behavior stays correct instead of panicking
//
// sync.Once means the zone is captured once per process; runtime TZ changes are
// intentionally not supported — restart the process to switch zones.
func Location() *time.Location {
	locOnce.Do(func() {
		tz := strings.TrimSpace(os.Getenv("TZ"))
		name := tz
		if name == "" {
			name = defaultZone
		}
		l, err := time.LoadLocation(name)
		if err != nil {
			log.Printf("[WARN] TZ=%q 加载失败, 已回退 UTC+8: %v", tz, err)
			l = time.FixedZone("CST", 8*60*60)
		}
		loc = l
	})
	return loc
}

// Now returns the current time in the configured zone. Use this everywhere
// instead of time.Now() / time.Now().UTC() so the whole pipeline shares one
// wall clock.
func Now() time.Time {
	return time.Now().In(Location())
}

// In converts t to the configured zone.
func In(t time.Time) time.Time {
	return t.In(Location())
}
