// Package timing provides a lightweight, append-only stage timer that records
// the duration of each major step of the smart-summary pipeline both to stdout
// (via the standard logger, alongside the existing "took %dms" lines) and to a
// dedicated timing log file.
//
// Each line in the file is a single, self-describing record:
//
//	2026-06-04T17:00:00+08:00 task_no=ST20260604abcd1234 stage=fetch_messages took_ms=1234
//
// The timestamp is in Asia/Shanghai (Beijing time) so the timing log agrees
// with the rest of the system's wall clock. The target directory is created on
// first use (os.MkdirAll) and the file is opened in append mode so concurrent
// workers and process restarts never truncate prior records.
package timing

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
)

// DefaultLogPath is the in-container path of the timing log. The directory is
// created automatically; mount it to the host (see deploy compose) if the log
// must survive container restarts.
const DefaultLogPath = "/var/log/smart-summary/timing.log"

var (
	mu       sync.Mutex
	file     *os.File
	filePath = DefaultLogPath
	openErr  error
	openOnce sync.Once
)

// SetLogPath overrides the timing log file path. Must be called before the first
// Record/Observe; safe no-op if the path is empty.
func SetLogPath(p string) {
	if p == "" {
		return
	}
	mu.Lock()
	filePath = p
	mu.Unlock()
}

// ensureFile lazily opens (creating parent dirs) the timing log file. Failures
// are logged once and degrade gracefully to stdout-only timing.
func ensureFile() *os.File {
	openOnce.Do(func() {
		mu.Lock()
		p := filePath
		mu.Unlock()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			openErr = err
			log.Printf("[timing] cannot create dir for %s: %v (timing file disabled)", p, err)
			return
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			openErr = err
			log.Printf("[timing] cannot open %s: %v (timing file disabled)", p, err)
			return
		}
		file = f
	})
	return file
}

// Record writes one stage timing record to both stdout and the timing file.
// taskNo identifies the summary task; stage is the pipeline step name; d is the
// measured duration.
func Record(taskNo, stage string, d time.Duration) {
	ms := d.Milliseconds()
	// Always echo to stdout so existing log-based observability still works.
	log.Printf("[timing] task=%s stage=%s took=%dms", taskNo, stage, ms)

	f := ensureFile()
	if f == nil {
		return
	}
	line := fmt.Sprintf("%s task_no=%s stage=%s took_ms=%d\n",
		timezone.Now().Format(time.RFC3339), taskNo, stage, ms)
	mu.Lock()
	_, _ = f.WriteString(line)
	mu.Unlock()
}

// Stage starts a timer for `stage`. Call the returned func (typically deferred)
// to record the elapsed duration. Example:
//
//	done := timing.Stage(taskNo, "llm_summary")
//	... work ...
//	done()
func Stage(taskNo, stage string) func() {
	start := time.Now()
	return func() {
		Record(taskNo, stage, time.Since(start))
	}
}

// Observe records a stage whose start time the caller already holds. Useful when
// the start instant predates the decision to measure.
func Observe(taskNo, stage string, start time.Time) {
	Record(taskNo, stage, time.Since(start))
}
