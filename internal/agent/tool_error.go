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
//   - request-scoped store capacity (typed)    → never retryable; fatal on a
//     critical tool (the cap is per-request, so no retry can clear it)
//   - permission / access / identity / auth  → never retryable; fatal only on a
//     critical tool (elsewhere it is a per-channel gap, not a failed run)
//   - explicit invalid args / parse           → retryable, not fatal
//   - anything else on a critical tool        → fatal AND retryable: completeness is
//     at risk if it is never recovered, but an unrecognised error is not known to
//     be permanent, and the fatal marker's mitigation IS the retry
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

	// SS-12-b coverage gate FIRST, on the typed error, before any substring
	// matching. summarize_chunk is a criticalTool, so a misclassification here
	// would mark the run FAILED and trade "incomplete summary" for "no summary".
	// Keying on the type rather than on the text also means a channel NAME
	// containing "permission" / "timeout" / "unauthorized" cannot steer the
	// verdict — the instruction embeds user-controlled channel names verbatim.
	// Retryable + NON-fatal is the contract the SS-12 §5 prompt acts on: the
	// planner fetches the named channels and re-calls this same tool.
	var gate *CoverageGateError
	if errors.As(err, &gate) {
		env.ErrorCode, env.Retryable, env.Fatal = "COVERAGE_INCOMPLETE", true, false
		return env
	}
	var outsideScope *ErrChannelOutsideSelectedScope
	if errors.As(err, &outsideScope) {
		env.ErrorCode, env.Retryable, env.Fatal = "CHANNEL_OUTSIDE_SELECTED_SCOPE", false, false
		return env
	}

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
	case errors.Is(err, ErrSummaryHandleCapacity):
		// A per-request store cap (handle count / total text bytes). Unlike every
		// other branch here this is matched by identity, not by substring, because
		// substring matching is what broke it: the store spells "summary handle"
		// with a space, the branch below matches "summary_handle" with an
		// underscore, so capacity errors fell through to the critical-tool default
		// and came back retryable=true. The nudged retry re-ran the same Map into
		// the same full store, every step, until MaxSteps ended the run with zero
		// output. NON-retryable because the caps are per-request and the store only
		// grows: no retry within this request can succeed. Fatal on a critical tool
		// because the chunk it could not store is coverage that is genuinely lost —
		// the run should end reporting that, not spin.
		env.ErrorCode, env.Retryable, env.Fatal = "RESOURCE_EXHAUSTED", false, criticalTools[toolName]
	case strings.Contains(low, "messages_handle") || strings.Contains(low, "summary_handle"):
		// A dropped/expired message-cache handle (`messages_handle`) or a bad
		// request-scoped Map result handle (`summary_handle`).
		// It is NOT a permission failure — the handle simply aged out of the
		// cache — so it must be caught before the permission branch below.
		// Retryable + non-fatal: re-minting the handle (list/fetch again) and
		// re-calling succeeds, and one stale handle must not mark the whole run
		// FAILED and suppress the retry whose success would clear it.
		env.ErrorCode, env.Retryable, env.Fatal = "INVALID_ARGUMENT", true, false
	case isTransientIdentityOutage(low):
		// `identity` can be either a real authorization invariant ("missing user
		// identity in context") or an upstream identity-service outage. The outage
		// spelling must be classified before the bare identity permission branch, or
		// a transient 503 becomes the last fatal+non-retryable self-sealing case.
		env.ErrorCode, env.Retryable, env.Fatal = "TRANSIENT_TOOL_ERROR", true, false
	case strings.Contains(low, "not accessible") || strings.Contains(low, "permission") ||
		strings.Contains(low, "access denied") || strings.Contains(low, "identity") ||
		strings.Contains(low, "unauthor") || strings.Contains(low, "forbidden"):
		// Never retryable — a permission denial cannot be retried into success —
		// but fatal only for RUN-scoped permission/auth failures. A channel/handle
		// permission denial is already captured by coverage as a failed target and
		// belongs in the finish gate as PARTIAL + GapChannel, not as a run-level
		// FAILED marker. That marker is unclearable by construction: a denied
		// channel will not later succeed, while other channels' successes use
		// different targets and correctly do not clear it.
		env.ErrorCode, env.Retryable, env.Fatal = "PERMISSION_DENIED", false, criticalTools[toolName] && !isChannelScopedPermissionDenial(toolName, low)
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
			// Retryable AND fatal is not a contradiction, it is the whole point:
			// the two fields answer different questions. Fatal says "if this is
			// never recovered, completeness is compromised"; retryable says "a
			// retry might recover it". An UNRECOGNISED error is by definition not
			// known to be permanent, so claiming retryable=false here asserted
			// knowledge the classifier does not have — and it sealed the branch
			// shut: this arm's own documented mitigation is that the marker clears
			// when the same tool later succeeds, which requires the retry that
			// retryable=false tells the model not to attempt. Every unlisted DB/RPC
			// wording (Too many connections, transport is closing, a bare upstream
			// 502) therefore turned a transient blip into a permanent FAILED.
			env.ErrorCode, env.Retryable, env.Fatal = "CRITICAL_TOOL_ERROR", true, true
		} else {
			env.ErrorCode, env.Retryable, env.Fatal = "TOOL_ERROR", true, false
		}
	}
	// Reduce is the completeness boundary. Any merge_summaries failure leaves
	// successful Map output without a validated final synthesis, so it must latch
	// the run as failed until a later merge succeeds. Retryability still follows
	// the classification above; OnToolSuccess clears the fatal marker.
	if toolName == "merge_summaries" {
		env.Fatal = true
	}
	return env
}

func isTransientIdentityOutage(msg string) bool {
	if !strings.Contains(msg, "identity") {
		return false
	}
	// A permanent permission denial that merely mentions identity together with a
	// weak transient term ("identity: permission denied, please try again later")
	// is NOT a transient outage. Classifying it transient makes the model retry a
	// denial that can never succeed until MaxSteps — the exact self-sealing case the
	// permission branch ordering exists to prevent (round-9 P2-1). Require the
	// ABSENCE of any permission word.
	if strings.Contains(msg, "permission") || strings.Contains(msg, "denied") ||
		strings.Contains(msg, "unauthor") || strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "not accessible") {
		return false
	}
	return strings.Contains(msg, "service unavailable") ||
		strings.Contains(msg, "try again") ||
		strings.Contains(msg, "retry later") ||
		containsHTTP5xx(msg)
}

// isChannelScopedPermissionDenial reports whether a permission-branch error is
// scoped to a single channel (→ non-fatal, disclosed by the finish gate as
// PARTIAL + GapChannel) rather than to the whole run.
//
// It is keyed on a FACT, not on error wording: fetch_channel is the only critical
// tool with a coverage recorder behind it, and EVERY fetch_channel error path
// calls recordFetch(false, …) (tool_fetch_channel.go), so a denied channel is
// always captured as a failed target. The tool identity is therefore the sound
// signal — `channel X not accessible`, `get user channels: permission denied`, and
// `fetch messages: forbidden` are all one denied channel, and keying on the tool
// covers them all without chasing each spelling.
//
// search_messages is deliberately excluded: it has NO coverage recorder, so a
// non-fatal-and-unrecorded classification would let the gate report COMPLETE —
// strictly worse than the FAILED it would replace (round-9 P1-2).
//
// A genuine RUN-scoped auth failure stays fatal: "missing user identity in
// context" is the whole run's identity, not one channel, so it is excluded.
func isChannelScopedPermissionDenial(toolName, msg string) bool {
	return toolName == "fetch_channel" && !strings.Contains(msg, "identity")
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
// than as an incidental number. It requires both a status-ish prefix (including
// `returned `, folded in from the old containsReturned5xx so identity/upstream
// outages spelled `... returned 503` are one list) AND that the code is not part
// of a longer number: `status 5000` is not a 500, and a channel id containing 429
// is not a throttle.
func containsHTTPStatus(msg, code string) bool {
	for _, prefix := range []string{"status=", "status ", "http ", "statuscode=", "status code ", "status_code=", "returned "} {
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
