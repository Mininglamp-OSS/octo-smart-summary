package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// ToolErrorEnvelope is the structured result a failed tool returns to the model
// (SS-07b), replacing the opaque "错误: <text>" string. It lets the planner
// reason about whether the failure is retryable or fatal instead of guessing
// from free text (defect #5), and lets the runner surface fatal failures to the
// finish gate.
type ToolErrorEnvelope struct {
	OK        bool   `json:"ok"`
	ErrorCode string `json:"error_code"`
	Retryable bool   `json:"retryable"`
	Fatal     bool   `json:"fatal"`
	Message   string `json:"message"`
}

// criticalTools are the tools whose failure compromises data completeness; a
// fatal error from them must block a COMPLETE verdict.
var criticalTools = map[string]bool{
	"fetch_channel":   true,
	"search_messages": true,
	"filter_relevant": true,
	"summarize_chunk": true,
	"merge_summaries": true,
}

// classifyToolError maps a tool failure to a structured envelope. The rules are
// deliberately conservative and text-based (the underlying tools return plain
// errors); grow them as tools adopt typed errors.
//
//   - handler panic                          → fatal internal error
//   - context deadline / canceled            → retryable, not fatal
//   - 429 / 5xx / network / DB connection    → retryable, not fatal
//   - stale/expired messages_handle           → retryable, not fatal (cache miss)
//   - permission / access / identity / auth  → fatal (cannot be retried into success)
//   - explicit invalid args / parse           → retryable, not fatal
//   - anything else on a critical tool        → fatal (data completeness at risk)
//   - anything else on a non-critical tool    → retryable
//
// The classification is advisory about RECOVERY, not authoritative: the run's
// fatal marker is cleared when the same tool later succeeds (Runner.OnToolSuccess).
// That matters because this list has been wrong in both directions on the same
// string — mysql's `invalid connection` was first swallowed by a bare "invalid"
// match, then promoted to a fatal critical-tool default — and no substring set
// can tell "the run ended here" from "the model retried and it worked". Patterns
// here should therefore aim at giving the MODEL a useful retryable hint; the
// verdict's correctness no longer rests on them alone.
func classifyToolError(toolName string, err error) ToolErrorEnvelope {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	low := strings.ToLower(msg)
	env := ToolErrorEnvelope{OK: false, Message: msg}

	switch {
	case strings.Contains(low, "panicked"):
		env.ErrorCode, env.Retryable, env.Fatal = "INTERNAL_ERROR", false, true
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(low, "deadline") || strings.Contains(low, "timeout"):
		env.ErrorCode, env.Retryable, env.Fatal = "TIMEOUT", true, false
	case errors.Is(err, context.Canceled) || strings.Contains(low, "canceled") || strings.Contains(low, "cancelled"):
		env.ErrorCode, env.Retryable, env.Fatal = "CANCELED", true, false
	case containsHTTPStatus(low, "429") || strings.Contains(low, "too many requests") || strings.Contains(low, "rate limit"):
		// Anchored like the 5xx check below. An unanchored "429" matched channel and
		// user ids interpolated into error text — `channel 1234297 not accessible by
		// user u-88` classified as RATE_LIMITED, so the model retried a permission
		// failure that can never succeed until MaxSteps ran out.
		env.ErrorCode, env.Retryable, env.Fatal = "RATE_LIMITED", true, false
	case containsHTTP5xx(low):
		env.ErrorCode, env.Retryable, env.Fatal = "UPSTREAM_ERROR", true, false
	case strings.Contains(low, "network") || strings.Contains(low, "dial tcp") ||
		strings.Contains(low, "connection refused") || strings.Contains(low, "connection reset") ||
		strings.Contains(low, "no such host") || strings.Contains(low, "temporary failure") ||
		strings.Contains(low, "driver: bad connection") || strings.Contains(low, "database is closed") ||
		strings.Contains(low, "db connection") || strings.Contains(low, "connection failed") ||
		// One class, one branch. These are all stale-pooled-connection or
		// interrupted-stream shapes; splitting them across branches is how
		// `driver: bad connection` ended up retryable while `invalid connection`
		// (go-sql-driver's ErrInvalidConn, the same situation) became fatal.
		strings.Contains(low, "invalid connection") ||
		strings.Contains(low, "connection is already closed") ||
		strings.Contains(low, "broken pipe") || strings.Contains(low, "unexpected eof") ||
		strings.Contains(low, "eof") && strings.Contains(low, "post ") ||
		strings.Contains(low, "deadlock found") || strings.Contains(low, "lock wait timeout"):
		// Strong, unambiguous transient shapes only. The weaker "service
		// unavailable" / "try again" terms moved BELOW the permission branch:
		// `permission denied; please try again later` carries both, and a stale
		// permission is fatal — retrying "try again" text can never clear it.
		env.ErrorCode, env.Retryable, env.Fatal = "TRANSIENT_TOOL_ERROR", true, false
	case strings.Contains(low, "messages_handle"):
		// A dropped/expired message-cache handle (`invalid or expired
		// messages_handle: h-123`, emitted on a plain cache miss by
		// tool_search_messages / tool_filter_relevant / tool_summarize_chunk).
		// It is NOT a permission failure — the handle simply aged out of the
		// cache — so it must be caught before the permission branch below.
		// Retryable + non-fatal: re-minting the handle (list/fetch again) and
		// re-calling succeeds, and one stale handle must not mark the whole run
		// FAILED and suppress the retry whose success would clear it.
		env.ErrorCode, env.Retryable, env.Fatal = "INVALID_ARGUMENT", true, false
	case strings.Contains(low, "not accessible") || strings.Contains(low, "permission") ||
		strings.Contains(low, "access denied") || strings.Contains(low, "identity") ||
		strings.Contains(low, "unauthor") || strings.Contains(low, "forbidden"):
		env.ErrorCode, env.Retryable, env.Fatal = "PERMISSION_DENIED", false, true
	case strings.Contains(low, "service unavailable") || strings.Contains(low, "try again"):
		// Weak transient terms, checked only after permission: a genuine
		// "service temporarily unavailable, try again" with no permission wording
		// is a retryable blip.
		env.ErrorCode, env.Retryable, env.Fatal = "TRANSIENT_TOOL_ERROR", true, false
	case strings.Contains(low, "parse args") || strings.Contains(low, "cannot parse") ||
		strings.Contains(low, "parsing time") || strings.Contains(low, "channel_type is required"):
		// A bad tool argument (e.g. the model sent an empty time_start, so
		// time.Parse fails with "cannot parse ...") is the agent's own mistake,
		// not a fatal system failure. Retryable so the agent fixes and re-calls;
		// NON-fatal so one arg slip does not mark the whole run failed and block
		// saving an otherwise-good summary (issue C).
		env.ErrorCode, env.Retryable, env.Fatal = "INVALID_ARGUMENT", true, false
	case strings.Contains(low, "persist evidence") || strings.Contains(low, "write evidence"):
		// Narrowed from a bare "evidence" match, which caught reads too:
		// `get session message pool: query evidence rows: ...` is a READ failure,
		// usually a one-second connection blip, and it was being reported as a fatal
		// evidence-WRITE failure. Reads fall through to the branches above (transient
		// connection shapes) or to the critical-tool default.
		env.ErrorCode, env.Retryable, env.Fatal = "EVIDENCE_WRITE_FAILED", false, true
	default:
		if criticalTools[toolName] {
			env.ErrorCode, env.Retryable, env.Fatal = "CRITICAL_TOOL_ERROR", false, true
		} else {
			env.ErrorCode, env.Retryable, env.Fatal = "TOOL_ERROR", true, false
		}
	}
	return env
}

func containsHTTP5xx(msg string) bool {
	for _, marker := range []string{"500", "501", "502", "503", "504", "505", "506", "507", "508", "510", "511"} {
		if containsHTTPStatus(msg, marker) {
			return true
		}
	}
	return false
}

// containsHTTPStatus reports whether msg carries `code` as an HTTP status rather
// than as an incidental number. It requires both a status-ish prefix AND that the
// code is not part of a longer number: `status 5000` is not a 500, and a channel
// id containing 429 is not a throttle.
func containsHTTPStatus(msg, code string) bool {
	for _, prefix := range []string{"status=", "status ", "http ", "statuscode=", "status code ", "status_code="} {
		from := 0
		for {
			i := strings.Index(msg[from:], prefix+code)
			if i < 0 {
				break
			}
			end := from + i + len(prefix) + len(code)
			if end >= len(msg) || msg[end] < '0' || msg[end] > '9' {
				return true
			}
			from = from + i + 1
		}
	}
	return false
}

// JSON renders the envelope as the tool result string fed back to the model.
func (e ToolErrorEnvelope) JSON() string {
	b, err := json.Marshal(e)
	if err != nil {
		return `{"ok":false,"error_code":"TOOL_ERROR","retryable":true,"fatal":false,"message":"marshal error"}`
	}
	return string(b)
}

// toolCallTarget extracts the per-call target that the fatal-marker set is keyed
// on (SS-07b), so a success on one channel/chunk does not clear a fatal marker
// left by a different channel/chunk running in parallel through the worker pool.
//
// It reads the arguments generically: channel_id identifies a fetch_channel call,
// messages_handle identifies a summarize_chunk / search_messages / filter_relevant
// call (each fan-out unit gets a distinct handle). Single-target tools carry
// neither and return "" — they key on the tool name alone, unchanged. A retry of
// the same target is a new tool_call with the SAME channel_id / handle, so the
// legitimate recover-on-success path still matches.
func toolCallTarget(arguments string) string {
	if arguments == "" {
		return ""
	}
	var a struct {
		ChannelID      string `json:"channel_id"`
		MessagesHandle string `json:"messages_handle"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return ""
	}
	if a.ChannelID != "" {
		return "channel_id=" + a.ChannelID
	}
	if a.MessagesHandle != "" {
		return "messages_handle=" + a.MessagesHandle
	}
	return ""
}
