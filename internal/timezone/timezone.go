// Package timezone centralizes the project's wall-clock reference so that the
// database, backend logic and frontend all agree on a single timezone:
// Asia/Shanghai (Beijing time, UTC+8).
//
// Background: the scheduler previously mixed time.Now().UTC() with a DSN using
// loc=Local (container TZ=Asia/Shanghai), causing an 8-hour skew between what
// the frontend selected (e.g. 17:00 Beijing) and when tasks actually fired.
// All "current time" reads must go through Now() so writes, reads, comparisons
// and schedule math share the same Asia/Shanghai zone.
package timezone

import (
	"sync"
	"time"
)

const Name = "Asia/Shanghai"

var (
	loc     *time.Location
	locOnce sync.Once
)

// Location returns the Asia/Shanghai location. If the zoneinfo database is
// unavailable it falls back to a fixed UTC+8 offset so behavior stays correct
// (China has no DST, so a fixed +8 offset is exact).
func Location() *time.Location {
	locOnce.Do(func() {
		l, err := time.LoadLocation(Name)
		if err != nil {
			l = time.FixedZone("CST", 8*60*60)
		}
		loc = l
	})
	return loc
}

// Now returns the current time in Asia/Shanghai. Use this everywhere instead of
// time.Now() / time.Now().UTC() so the whole pipeline shares one wall clock.
func Now() time.Time {
	return time.Now().In(Location())
}

// In converts t to Asia/Shanghai.
func In(t time.Time) time.Time {
	return t.In(Location())
}
