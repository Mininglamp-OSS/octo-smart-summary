package timezone

import (
	"bytes"
	"log"
	"os"
	"sync"
	"testing"
	"time"
)

// resetLocation clears the sync.Once cache so each case re-resolves TZ from
// scratch; without it Location() would return the first case's zone forever.
func resetLocation(t *testing.T) {
	t.Helper()
	loc = nil
	locOnce = sync.Once{}
}

// 验证默认行为不变: 未设 TZ 时仍解析为 Asia/Shanghai。
func TestLocation_DefaultShanghai(t *testing.T) {
	resetLocation(t)
	t.Setenv("TZ", "")
	if got := Location().String(); got != "Asia/Shanghai" {
		t.Fatalf("default zone: got %q, want Asia/Shanghai", got)
	}
}

// 验证合法 TZ 被采用: 覆盖默认, 解析成对应 zone。
func TestLocation_ValidTZ(t *testing.T) {
	resetLocation(t)
	t.Setenv("TZ", "America/New_York")
	if got := Location().String(); got != "America/New_York" {
		t.Fatalf("valid TZ: got %q, want America/New_York", got)
	}
}

// 验证非法 TZ 走兜底: 不 panic, 且落到固定 UTC+8 偏移(中国无 DST, +8 精确)。
func TestLocation_InvalidTZFallsBackToUTC8(t *testing.T) {
	resetLocation(t)
	t.Setenv("TZ", "Invalid/NotAZone")
	_, offset := time.Now().In(Location()).Zone()
	if offset != 8*60*60 {
		t.Fatalf("invalid TZ fallback offset: got %d, want %d", offset, 8*60*60)
	}
}

// 验证兜底路径打出 WARN 日志: 原静默吞错已按老板要求改为可排查的告警。
func TestLocation_InvalidTZLogsWarn(t *testing.T) {
	resetLocation(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	t.Setenv("TZ", "Invalid/NotAZone")
	_ = Location()

	if !bytes.Contains(buf.Bytes(), []byte("[WARN]")) || !bytes.Contains(buf.Bytes(), []byte("TZ=")) {
		t.Fatalf("expected WARN log with [WARN] and TZ= markers, got: %q", buf.String())
	}
}
