package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// SS-01 止血常量。
//
// 历史 bug：chunk_size 默认 500，但 formatChunkForLLM 每片只格式化前 200 条，
// 满片时静默丢弃 300/500（60%）。SS-01 先把默认值与硬上限收敛到 200 止血；
// SS-06b 随后引入 token-aware 分片并删除了 200 格式化上限，分片改由 token 预算
// （按渲染行计费）+ hardMessageBackstop 双重约束。
//
// #241 item 2 之后覆盖不再恒为 100%：单块 LLM 调用失败会被容忍并从 reduce 输入
// 剔除，且扇出超过 maxChunkCalls 时截断最旧的分片——两种缺口都经 cov
// （failed_chunk_count / chunk_calls_capped / dropped_count / truncated）披露，
// 并在合并文本里追加一条 mapCoverageGapNotice 作为尽力而为的提示——它是 Reduce
// 输入，会经 merge/planner 两次模型改写，模型可能删掉；真正模型删不掉的结构性披露
// （recordDroppedMessages → finishgate PARTIAL）仍取决于 AGENT_SUMMARY_V2_MODE≠off。
// V2=off 回落下只剩这条可被改写的提示，其模型无关的结构通道见后续 issue #267。
const (
	// defaultChunkSize 是 chunk_size 缺省（<=0）时使用的每片消息数基准，
	// 与 SS-01 止血值一致。
	defaultChunkSize = 200
	// oversizedMessageRunes 是单条消息被标记为“超长”的字符阈值。超长消息只
	// 计数上报（oversized_message_count），不做静默截断——“永不丢消息、永不
	// 切断单条消息”是 SS-06b 的刻意取舍（PR #196 review P2-3）：超过整片预算
	// 的消息单独成片。阈值只服务于可观测性上报，与分片预算
	// （ResolveMapMaxTokens 量级）无关。
	oversizedMessageRunes = 4000
	// maxChunkCalls caps the number of LLM calls one summarize_chunk invocation
	// may fan out (#241 item 2). Chunk COUNT was previously unbounded — with
	// MaxSafetyLimit=100000 messages upstream a single call could dispatch
	// hundreds of LLM requests with no wall-clock or cost bound. Beyond this the
	// input splits are TRUNCATED to the most recent maxChunkCalls (not rejected:
	// a rejection latches the run FAILED with no planner recovery), and the drop
	// is disclosed via cov.ChunkCallsCapped + the inline mapCoverageGapNotice.
	maxChunkCalls = 256
)

// mapCoverageGapNotice is appended to the combined Map output whenever coverage
// is incomplete for ANY reason — a tolerated per-chunk failure, a blank-but-
// successful chunk, a message dropped after the citation manifest froze, or the
// maxChunkCalls fan-out truncation — so the gap is disclosed IN the summary text,
// independent of the V2 run-row disclosure (recordDroppedMessages → finishgate
// PARTIAL), which ships dark by default, meaning the combined text alone would
// otherwise read complete (#256 P1). merge_summaries treats it as ordinary text;
// the reduce prompt (prompts/summary.md) lists the coverage fields the planner
// must act on.
const mapCoverageGapNotice = "\n\n---\n\n（注意：部分聊天内容未能纳入本次总结，结果可能不完整。）"

// chunkCoverage 汇总 summarize_chunk 实际喂给模型的消息覆盖情况，随工具结果
// 返回，让 Runner/Planner 能判断是否发生丢弃或截断，而不是只看到 chunk_count。
type chunkCoverage struct {
	InputCount     int `json:"input_count"`
	ProcessedCount int `json:"processed_count"`
	// DroppedCount is ACCIDENTAL/silent loss only: messages not represented in
	// the Map output because their chunk failed, returned blank, or was fetched
	// after the citation manifest froze. Intentional fan-out-cap drops live in
	// CappedDroppedCount instead — a disclosed cap is not a silent loss — so a
	// "did we lose data unexpectedly?" check keys on this field alone (#256 P2).
	DroppedCount int `json:"dropped_count"`
	// CappedDroppedCount is the messages dropped by the maxChunkCalls fan-out cap
	// (#241). It is intentional and disclosed via ChunkCallsCapped, and is kept
	// OUT of DroppedCount so it does not read as accidental loss (#256 P2).
	CappedDroppedCount    int `json:"capped_dropped_count"`
	OversizedMessageCount int `json:"oversized_message_count"`
	FailedChunkCount      int `json:"failed_chunk_count"`
	// BlankChunkCount is chunks whose Map LLM call SUCCEEDED but returned an empty
	// summary — the model considered the messages and found nothing worth
	// reporting (it is told to skip off-topic chatter). Those messages are COVERED
	// (counted in ProcessedCount), not lost; BlankChunkCount is observability only
	// and must not assert incompleteness (contract §3/§4).
	BlankChunkCount  int  `json:"blank_chunk_count"`
	ChunkCallsCapped bool `json:"chunk_calls_capped"`
	Truncated        bool `json:"truncated"`
	ChunkSize        int  `json:"chunk_size"`
}

// finalizeDropCounts derives DroppedCount and Truncated from the counters the
// Map phase accumulated (ProcessedCount, CappedDroppedCount). It must run after
// the Map phase and after CappedDroppedCount is set.
//
//   - DroppedCount is ACCIDENTAL loss only: everything not processed minus the
//     intentional, disclosed fan-out-cap drops (#256 P2-4).
//   - Truncated is the loudest model-facing coverage boolean, so it flags ANY
//     real gap — accidental OR the cap. It must NOT go quiet on the single
//     largest loss this tool can produce (a cap dropping up to ~half the input);
//     the accidental/intentional split lives in the two counters and the SS-02
//     probe, not here (#256 round-7 P2-1).
func (cov *chunkCoverage) finalizeDropCounts() {
	cov.DroppedCount = cov.InputCount - cov.ProcessedCount - cov.CappedDroppedCount
	cov.Truncated = cov.DroppedCount > 0 || cov.CappedDroppedCount > 0
}

// clampChunkSize 把请求的 chunk_size 收敛到 [1, hardMessageBackstop]；<=0 取
// defaultChunkSize，超过上限截到 hardMessageBackstop。chunk_size 是模型给的
// 工具参数，未经验证的值不得原样影响出站请求规模（PR #196 review P1-1）；
// 返回值即 splitMsgMapsByTokenBudget 的 maxMsgs，保证每片消息数有硬上限。
func clampChunkSize(requested int) int {
	if requested <= 0 {
		return defaultChunkSize
	}
	if requested > hardMessageBackstop {
		return hardMessageBackstop
	}
	return requested
}

// ProbeChunkCoverage drives the PRODUCTION chunking path — clampChunkSize ->
// splitMsgMapsByTokenBudget -> formatChunkForLLM — over msgMaps without any
// LLM call, and reports the coverage funnel the model would actually receive:
// processed = lines the formatter really emitted, dropped = input − processed,
// chunks = number of chunks the splitter produced for the given budget and
// ratios, capped = messages the fan-out cap intentionally discarded (a subset of
// dropped). The eval gate (SS-02) subtracts capped, so it treats a disclosed cap
// as no loss while STILL failing on a genuine splitter/formatter regression:
// capped counts ONLY the chunks capChunks actually removed, and only when the cap
// fired. Deriving it unconditionally as input−kept would misattribute splitter-
// class loss (a message that never reached any chunk) to the intentional-cap
// counter, cancelling it in dropped−capped and masking a real regression (#256 r8
// §3). Matches production, where CappedDroppedCount is likewise gated on the cap
// flag and kept out of the silent-loss signal (#256 P2).
//
// Replaces the arithmetic-only ComputeCoverage, which returned a hardcoded
// dropped=0 and never touched the splitter or formatter — a guard that
// cannot fail is worse than no guard (PR #196 review P1-2).
func ProbeChunkCoverage(msgMaps []map[string]interface{}, requestedChunkSize, budget, cjkRatio, asciiRatio int) (processed, dropped, chunks, capped int) {
	if len(msgMaps) == 0 {
		return 0, 0, 0, 0
	}
	size := clampChunkSize(requestedChunkSize)
	chunked := splitMsgMapsByTokenBudget(msgMaps, budget, size, cjkRatio, asciiRatio)
	// Count the messages the splitter emitted BEFORE the cap, so capped can be the
	// exact size of the cap-removed tail rather than input−kept (which would also
	// swallow any splitter loss).
	preCapMsgs := 0
	for _, c := range chunked {
		preCapMsgs += len(c)
	}
	// The fan-out cap (#241) is part of the SHIPPED chunking chain, so the probe
	// must apply it too — otherwise the SS-02 gate would miss a regression that
	// only manifests past the cap. capChunks keeps the kept chunks; the dropped
	// tail's messages never get counted into processed, so they show up in dropped.
	chunked, capFired := capChunks(chunked)
	kept := 0
	for _, c := range chunked {
		_, p, _ := formatChunkForLLM(c)
		processed += p
		kept += len(c)
	}
	// capped = the cap-removed tail (preCapMsgs − kept), ONLY when the cap fired.
	// Splitter loss (input − preCapMsgs) is NOT in capped, so it stays in dropped
	// and dropped−capped catches it.
	if capFired {
		capped = preCapMsgs - kept
	}
	return processed, len(msgMaps) - processed, len(chunked), capped
}

// ProbeChunkCoverageDefault runs ProbeChunkCoverage across BOTH ratio sets the
// deployment can resolve — cjk=1 (generic models) and cjk=2 (the ratio
// ResolveCharsPerTokenCJK picks for qwen/deepseek/kimi when the env var is
// unset) — and reports the worst case. Round-3 review (yujiawei, review
// 4960791240): the gate previously pinned CharsPerTokenCJK: 1 only, so it
// never exercised the production ratio; "a gate that probed the production
// ratio ... would have caught it." The no-drop assertion is ratio-independent,
// so processed/dropped agree across ratios; chunks and capped report the maximum
// (the worst fan-out). Gates that must not depend on injected deps — the eval
// harness, CI — probe through this.
func ProbeChunkCoverageDefault(msgMaps []map[string]interface{}, requestedChunkSize int) (processed, dropped, chunks, capped int) {
	for _, cjk := range []int{1, 2} {
		cfg := config.Config{CharsPerTokenCJK: cjk, CharsPerTokenASCII: 4}
		p, d, c, capd := ProbeChunkCoverage(msgMaps, requestedChunkSize, chunkTokenBudget(cfg), cfg.ResolveCharsPerTokenCJK(), cfg.CharsPerTokenASCII)
		if p < processed || processed == 0 {
			processed = p
		}
		if d > dropped {
			dropped = d
		}
		if c > chunks {
			chunks = c
		}
		if capd > capped {
			capped = capd
		}
	}
	return processed, dropped, chunks, capped
}

// getSessionMessagePool retrieves all messages from all tool calls in the session,
// sorts them globally by timestamp, and assigns CitationIndex.
// This ensures the same global ordering that buildCitationsForSession will use.
//
// Handle discovery (post-#158 regression fix, 4-reviewer P1):
// Previously handles were discovered by querying agent_message WHERE
// role='tool'. That table is populated only by AppendMessages, which runs
// AFTER RunWithHistory returns. During the run itself — including when
// summarize_chunk executes inside the agent loop — there are zero tool rows
// for the current turn, so the pool was empty, CitationIndex stayed at the
// Go zero value 0, the LLM emitted `[0]` markers, and worker.BuildCitations
// (idx >= 1 && idx <= maxIdx) discarded every marker at save time. First-
// turn agent summaries produced broken/empty citations on the dominant path.
//
// The fix sources handles from agent_message_evidence instead. Evidence rows
// are written synchronously by PersistEvidence inside fetch_channel /
// peek_channel tool handlers (BEFORE the tool returns), so by the time
// summarize_chunk runs in a later step, the evidence table already contains
// every handle produced this turn. The messages themselves come from the
// in-memory cache when warm, and fall back to the evidence row's JSON
// snapshot when cold — byte-for-byte the same recovery
// buildCitationsForSession performs. Keeping the two symmetric guarantees
// the pre-assigned CitationIndex here matches the post-assignment there
// for both first-turn and long-running/paused sessions.
//
// Two-phase pool invariant (#161 P2, yujiawei):
// The pre-assigned CitationIndex here (mid-run) and the rebuilt index in
// buildCitationsForSession (save-time) are only guaranteed to match if the
// evidence row set does not change between the two phases. In practice this
// is upheld by both the profile ordering `fetch/search/filter →
// summarize_chunk → merge_summaries → answer` and Runner.runTools' causal
// barrier: when fetch_channel and summarize_chunk occur in one planner turn,
// every fetch completes (including evidence persistence) before any summarize
// starts. Thus no same-turn fetch can appear only at save time and shift the
// [n]-marker alignment.
func getSessionMessagePool(sessionID, uid string) ([]pipeline.Message, error) {
	summaryDB, _, _, _ := GetSummaryDeps()

	// Discover handles from evidence table — populated synchronously by
	// PersistEvidence inside fetch_channel / peek_channel before the tool
	// returns, so it is populated during the run (unlike agent_message which
	// is only written by AppendMessages after RunWithHistory returns).
	var evidenceRows []model.AgentMessageEvidence
	if err := summaryDB.Where("user_id = ? AND session_id = ?", uid, sessionID).
		Order("created_at ASC, handle ASC").
		Find(&evidenceRows).Error; err != nil {
		return nil, fmt.Errorf("query evidence rows: %w", err)
	}

	// Collect all messages from all handles (cache-hot preferred, evidence
	// JSON snapshot as fallback for cold cache / restart)
	var allMessages []pipeline.Message
	seenKey := make(map[string]bool) // de-dup by channel_id+message_seq

	cache := GetMessageCache()
	for _, ev := range evidenceRows {
		if ev.Handle == "" {
			continue
		}

		// Prefer cache (avoids JSON unmarshal on the hot path)
		if cached := cache.Retrieve(ev.Handle, uid, sessionID); cached != nil {
			for _, msg := range cached {
				key := fmt.Sprintf("%s:%d", msg.ChannelID, msg.MessageSeq)
				if !seenKey[key] {
					allMessages = append(allMessages, msg)
					seenKey[key] = true
				}
			}
			continue
		}

		// Cache miss: unmarshal the evidence JSON snapshot. Same recovery
		// path as buildCitationsForSession — see agent_summary_citations.go.
		var evidenceMessages []pipeline.Message
		if err := json.Unmarshal([]byte(ev.Evidence), &evidenceMessages); err != nil {
			continue
		}
		for _, msg := range evidenceMessages {
			key := fmt.Sprintf("%s:%d", msg.ChannelID, msg.MessageSeq)
			if !seenKey[key] {
				allMessages = append(allMessages, msg)
				seenKey[key] = true
			}
		}
	}

	// Sort by timestamp ascending, with (ChannelID, MessageSeq) as deterministic
	// tiebreaker. Must stay byte-identical to the sort in
	// agent_summary_citations.go:120-122 so that the pre-assigned CitationIndex
	// here matches the post-assignment there — see SUM-47 v3 rationale.
	sort.Slice(allMessages, func(i, j int) bool {
		if allMessages[i].Timestamp != allMessages[j].Timestamp {
			return allMessages[i].Timestamp < allMessages[j].Timestamp
		}
		if allMessages[i].ChannelID != allMessages[j].ChannelID {
			return allMessages[i].ChannelID < allMessages[j].ChannelID
		}
		return allMessages[i].MessageSeq < allMessages[j].MessageSeq
	})

	// Assign global CitationIndex (same as agent_summary_citations.go:102-103)
	for i := range allMessages {
		allMessages[i].CitationIndex = i + 1
	}

	return allMessages, nil
}

// SummarizeChunkTool generates a summary for a chunk of cached messages.
func SummarizeChunkTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "summarize_chunk",
			Description: "对缓存中的一批消息进行局部总结（Map 阶段）。正文保存在本次请求内，返回 summary_handle 和覆盖统计；后续 merge_summaries 必须传 handle，不要复制正文。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"messages_handle": map[string]interface{}{
						"type":        "string",
						"description": "消息缓存句柄",
					},
					"chunk_size": map[string]interface{}{
						"type":        "integer",
						"description": fmt.Sprintf("可选：每片最大消息数（叠加在 token 预算之上，取值收敛到 [1, %d]，<=0 按 %d）；分片同时受 token 预算与消息数双重约束，且单次调用最多 %d 个分片，超出时保留最近的分片、丢弃更早的（chunk_calls_capped=true）。返回值含 input_count/processed_count/dropped_count/capped_dropped_count/oversized_message_count/failed_chunk_count/blank_chunk_count/chunk_calls_capped/truncated/chunk_size；truncated 或 failed_chunk_count>0 或 chunk_calls_capped 时表示覆盖不完整。dropped_count 只计意外丢失（分块失败/空结果/引用清单外），capped_dropped_count 单列因分片上限有意丢弃的消息数。", hardMessageBackstop, defaultChunkSize, maxChunkCalls),
					},
				},
				"required": []string{"messages_handle"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			MessagesHandle string `json:"messages_handle"`
			ChunkSize      int    `json:"chunk_size,omitempty"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		// Extract uid from context
		uidVal := ctx.Value(ContextKeyUID)
		uid, ok := uidVal.(string)
		if !ok || uid == "" {
			return "", fmt.Errorf("missing user identity in context")
		}

		// Extract sessionID from context
		sessionIDVal := ctx.Value(ContextKeySessionID)
		sessionID, ok := sessionIDVal.(string)
		if !ok || sessionID == "" {
			return "", fmt.Errorf("missing session_id in context")
		}

		runID, _ := ctx.Value(ContextKeyRunID).(string)

		// SS-12-b: the coverage gate runs BEFORE anything can freeze the citation
		// manifest. If the run still owes an expected-channel fetch, the freeze must
		// not happen yet — a channel fetched after it is not citable under it, so
		// this is the last moment at which demanding the fetch is still free. The
		// error is retryable + non-fatal (see classifyToolError's CoverageGateError
		// arm) so the planner fetches and re-calls this same tool, and it is bounded
		// so an unfetchable channel cannot loop: after the cap the gate stops firing
		// and the freeze proceeds, leaving the finish gate to disclose the gap.
		// Placed before the cache Retrieve so a stale handle cannot mask a real
		// coverage failure with an unrelated INVALID_ARGUMENT.
		if err := checkCoverageBeforeFreeze(ctx, uid, sessionID, runID); err != nil {
			return "", err
		}

		messages := messageCache.Retrieve(req.MessagesHandle, uid, sessionID)
		if messages == nil {
			return "", fmt.Errorf("invalid or expired messages_handle: %s", req.MessagesHandle)
		}

		if len(messages) == 0 {
			// Same result shape as the normal path (PR #196 review P2-5,
			// closed in round-3): the planner must see one stable contract on
			// both branches, so the empty branch carries chunk_size too.
			return marshalSummarizeChunkResult(ctx, "无可总结内容", 0, chunkCoverage{})
		}

		// Get the global message pool for the session and pre-assign CitationIndex.
		// This ensures LLM sees the same indexes that buildCitationsForSession will assign.
		globalPool, err := getSessionMessagePool(sessionID, uid)
		if err != nil {
			return "", fmt.Errorf("get session message pool: %w", err)
		}

		// SS-05 B1: when V2 mode is on and a run is in scope, override the just-
		// computed indexes with the run's FROZEN manifest ordinals so the mid-run
		// and save-time citation passes cannot drift. Off / no run → unchanged.
		v2 := SummaryV2Enabled() && runID != ""
		if v2 {
			globalPool = applyFrozenManifest(ctx, uid, sessionID, runID, globalPool)
		}
		setSummaryCitationWindow(ctx, globalPool)

		// Build a map from (channel_id, message_seq) to CitationIndex
		citationMap := make(map[string]int)
		for _, msg := range globalPool {
			key := fmt.Sprintf("%s:%d", msg.ChannelID, msg.MessageSeq)
			citationMap[key] = msg.CitationIndex
		}

		inputCount := len(messages)

		// Apply the global CitationIndex to our messages. Under V2 the pool
		// is the FROZEN manifest, so a message fetched AFTER the freeze is not
		// in the map — it must not reach the model with its zero CitationIndex
		// (the model would be told to cite message [0]): drop it from the
		// chunk input instead and count the drop. On the legacy path every
		// cached message is in the pool, so this never fires there.
		messages, manifestMisses := assignCitationIndexes(messages, citationMap, v2)
		if manifestMisses > 0 {
			log.Printf("[summarize_chunk] session=%s run=%s: dropped %d message(s) fetched after the citation manifest was frozen (not citable under it)",
				sessionID, runID, manifestMisses)
		}
		if len(messages) == 0 {
			if manifestMisses > 0 {
				recordDroppedMessages(ctx, uid, runID, manifestMisses)
				return marshalSummarizeChunkResult(ctx, "无可总结内容", 0, chunkCoverage{
					InputCount:   inputCount,
					DroppedCount: manifestMisses,
					Truncated:    true,
					ChunkSize:    clampChunkSize(req.ChunkSize),
				})
			}
			return marshalSummarizeChunkResult(ctx, "无可总结内容", 0, chunkCoverage{})
		}

		// Convert to map format for the token-budget splitter.
		msgMaps := make([]map[string]interface{}, len(messages))
		for i, msg := range messages {
			msgMaps[i] = map[string]interface{}{
				"sender_name":    msg.SenderName,
				"content":        msg.Content,
				"timestamp":      msg.SendTime,
				"channel_id":     msg.ChannelID,
				"citation_index": msg.CitationIndex, // Global index
			}
		}

		// SS-06b: token-aware chunking. Each chunk is bounded by a token budget
		// that bills the RENDERED prompt lines (balanced by content — defect
		// #9's uneven-length problem) instead of a fixed message count, and the
		// per-chunk format cap is gone, so no message is ever dropped.
		// chunk_size is clamped to [1, hardMessageBackstop] before use
		// (PR #196 review P1-1): it is a model-supplied argument, and the
		// schema documents a bound the handler must actually enforce.
		_, _, _, cfg := GetSummaryDeps()
		budget := chunkTokenBudget(cfg)
		msgsPerChunk := clampChunkSize(req.ChunkSize)
		chunks := splitMsgMapsByTokenBudget(msgMaps, budget, msgsPerChunk, cfg.ResolveCharsPerTokenCJK(), cfg.CharsPerTokenASCII)

		// Bound the LLM-call fan-out (#241 item 2). Beyond maxChunkCalls, TRUNCATE
		// rather than rejecting the whole call (a rejection is a critical-tool
		// error that latches the run FAILED with no planner recovery). capChunks
		// keeps the MOST RECENT chunks (see its doc); the drop is disclosed below.
		chunks, capped := capChunks(chunks)
		cappedDropped := 0
		if capped {
			// Count the messages the cap discarded. chunks partitions msgMaps, so
			// the kept messages plus the capped-away tail sum to len(msgMaps); the
			// difference is the intentional drop, reported separately from the
			// silent-loss DroppedCount (#256 P2-4).
			keptMsgs := 0
			for _, c := range chunks {
				keptMsgs += len(c)
			}
			cappedDropped = len(msgMaps) - keptMsgs
			log.Printf("[summarize_chunk] fan-out capped at %d chunks; kept the most recent, dropped the oldest chunks (%d messages), disclosed via chunk_calls_capped (#241)", maxChunkCalls, cappedDropped)
		}

		// Summarize each chunk and aggregate honest coverage counts.
		cov := chunkCoverage{InputCount: inputCount, ChunkSize: msgsPerChunk, ChunkCallsCapped: capped, CappedDroppedCount: cappedDropped}
		// SS-06: load the run's SummarySpec-derived guidance once so every Map
		// call summarizes toward the user's actual requirements. Empty when V2 is
		// off / no run / no spec → legacy generic prompt.
		specGuidance := loadRunSpecGuidance(ctx)
		mapStart := time.Now()
		summaries, err := summarizeChunksConcurrently(ctx, chunks, specGuidance, &cov)
		if err != nil {
			return "", err
		}
		// summarize_chunk is itself an LLM pipeline, so its own cost needs a
		// breakdown in the run trace — otherwise it shows up as one opaque
		// tool span and a slow Map is indistinguishable from a slow planner.
		TraceFromContext(ctx).AddSubPhase(fmt.Sprintf("map(%dchunks)", len(chunks)),
			time.Since(mapStart).Milliseconds())
		cov.finalizeDropCounts()

		// The gap notice fires for ANY real coverage gap — accidental (a failed or
		// blank chunk, or a manifest-miss, all folded into DroppedCount) OR the
		// intentional cap — so the combined text discloses incompleteness whatever
		// the cause (#256 P1).
		coverageGap := cov.DroppedCount > 0 || cov.CappedDroppedCount > 0
		combinedSummary, err := assembleMapOutput(summaries, coverageGap)
		if err != nil {
			// Keep the coverage breakdown on the no-usable-output error so the cause
			// stays legible in logs/triage even when no chunk returned an error
			// (all-blank case), restoring the attempted/failed/blank detail (#256 P2).
			return "", fmt.Errorf("%w (input=%d processed=%d failed=%d blank=%d capped=%d)",
				err, cov.InputCount, cov.ProcessedCount, cov.FailedChunkCount, cov.BlankChunkCount, cov.CappedDroppedCount)
		}
		// Persist the dropped-message count only once we have a usable Map output
		// to return. recordDroppedMessages does `dropped_messages = dropped_messages
		// + ?`, so recording it before the error return above would double-count on
		// every retry of an all-blank Map (#256 P2). Record the TOTAL loss
		// (accidental + capped) so the finish-gate PARTIAL disclosure still reflects
		// every message the run did not cover, even though the two are tracked
		// separately in the coverage result.
		recordDroppedMessages(ctx, uid, runID, cov.DroppedCount+cov.CappedDroppedCount)
		// chunk_count reports the summaries actually stored (successful chunks),
		// not attempted, so it does not over-report by FailedChunkCount.
		return marshalSummarizeChunkResult(ctx, combinedSummary, len(summaries), cov)
	}

	return schema, handler
}

type summarizeChunkToolResult struct {
	SummaryHandle         string `json:"summary_handle"`
	ChunkCount            int    `json:"chunk_count"`
	InputCount            int    `json:"input_count"`
	ProcessedCount        int    `json:"processed_count"`
	DroppedCount          int    `json:"dropped_count"`
	CappedDroppedCount    int    `json:"capped_dropped_count"`
	OversizedMessageCount int    `json:"oversized_message_count"`
	FailedChunkCount      int    `json:"failed_chunk_count"`
	BlankChunkCount       int    `json:"blank_chunk_count"`
	ChunkCallsCapped      bool   `json:"chunk_calls_capped"`
	Truncated             bool   `json:"truncated"`
	ChunkSize             int    `json:"chunk_size"`
}

func marshalSummarizeChunkResult(ctx context.Context, summary string, chunkCount int, cov chunkCoverage) (string, error) {
	store, err := summaryHandleStoreFromContext(ctx)
	if err != nil {
		return "", err
	}
	handle, err := store.PutAtStep(summary, chunkCount, summaryToolStepFromContext(ctx))
	if err != nil {
		return "", fmt.Errorf("store summary result: %w", err)
	}
	data, err := json.Marshal(summarizeChunkToolResult{
		SummaryHandle:         handle,
		ChunkCount:            chunkCount,
		InputCount:            cov.InputCount,
		ProcessedCount:        cov.ProcessedCount,
		DroppedCount:          cov.DroppedCount,
		CappedDroppedCount:    cov.CappedDroppedCount,
		OversizedMessageCount: cov.OversizedMessageCount,
		FailedChunkCount:      cov.FailedChunkCount,
		BlankChunkCount:       cov.BlankChunkCount,
		ChunkCallsCapped:      cov.ChunkCallsCapped,
		Truncated:             cov.Truncated,
		ChunkSize:             cov.ChunkSize,
	})
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return string(data), nil
}

func recordDroppedMessages(ctx context.Context, uid, runID string, count int) {
	if count <= 0 || !SummaryV2Enabled() || uid == "" || runID == "" {
		return
	}
	summaryDB, _, _, _ := GetSummaryDeps()
	if summaryDB == nil {
		return
	}
	if err := summaryrun.NewStore(summaryDB).AddDroppedMessages(ctx, uid, runID, count); err != nil {
		log.Printf("[summarize_chunk] record dropped messages failed run=%s dropped=%d: %v", runID, count, err)
	}
}

// formatChunkForLLM renders a chunk into the "[n] sender: content" lines fed to
// the LLM. It is a pure function (no LLM, no I/O) so coverage can be asserted
// deterministically in tests / the eval harness without a live model.
//
// It returns how many messages it formatted (processed) and how many exceeded
// oversizedMessageRunes (oversized). Oversized messages are counted but NOT
// truncated. SS-06b removed the per-chunk message cap: chunks are bounded by a
// token budget upstream (splitMsgMapsByTokenBudget), which bills these exact
// rendered lines via renderMessageLine (PR #196 review P0-1) — so formatting
// every message in the chunk cannot overflow the budget, and processed always
// == len(chunk), i.e. no silent drop.
func formatChunkForLLM(chunk []map[string]interface{}) (formatted string, processed, oversized int) {
	var b strings.Builder
	for _, msg := range chunk {
		content, _ := msg["content"].(string)
		if len([]rune(content)) > oversizedMessageRunes {
			oversized++
		}
		b.WriteString(renderMessageLine(msg))
		processed++
	}
	return b.String(), processed, oversized
}

// summarizeMessagesChunk builds a structured prompt from msgMap chunk and calls LLM.
// Returns the summary plus how many messages were processed / were oversized, so
// the caller can aggregate honest coverage across chunks.
//
// specGuidance (SS-06) is the SummarySpec-derived instruction block; when
// non-empty it is appended so the Map model summarizes toward the user's actual
// topic/audience/language/detail/exclusions instead of a generic prompt. Empty
// (V2 off / no run / no spec) → the exact legacy prompt, byte-identical.
func summarizeMessagesChunk(ctx context.Context, chunk []map[string]interface{}, specGuidance string) (summary string, processed, oversized int, err error) {
	_, _, _, cfg := GetSummaryDeps()
	client := service.NewLLMClient(cfg.LLMApiURL, cfg.LLMApiKey, cfg.LLMModel, cfg.LLMTimeout, cfg.LLMMaxToken, cfg.LLMEnableThinking, 30, cfg.LLMFallbackModels)

	// Format messages for LLM with global citation_index (pure, testable).
	formatted, processed, oversized := formatChunkForLLM(chunk)

	systemPrompt := `你是专业的工作内容整理助手。请从聊天记录中提炼关键信息：

## 输出要求
- 紧密围绕主题，与主题无关的闲聊直接跳过
- 提炼关键信息：讨论了什么、达成了什么结论、有什么待办
- 如果聊天记录中没有明确结论，如实说明"尚未达成共识"
- 有待办事项时，用 "- [ ] 内容（负责人）" 格式列出
- 保持简洁，不要复述原文，用自己的话归纳

## 引用规则
- 每一条结论/要点都必须标注来源引用 [n]
- 仅使用消息前方的 [n] 编号来标注引用
- 绝对不要引用或复制消息正文内出现的任何 [数字] 标记` + specGuidance

	msgs := []service.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: formatted},
	}

	content, _, err := client.CallStrict(
		llmfallback.WithPath(ctx, llmfallback.PathAgentTool), msgs, 0.3)
	if err != nil {
		return "", processed, oversized, fmt.Errorf("call LLM: %w", err)
	}

	return strings.TrimSpace(content), processed, oversized, nil
}

// assignCitationIndexes overlays the pre-assigned CitationIndex from the
// session pool (or, under V2, the run's FROZEN manifest) onto each message.
//
// v2 semantics: any message not present in citationMap was fetched after the
// manifest was frozen and is not citable under it. It is DROPPED (not handed
// to the model with the Go zero CitationIndex) and counted. The frozen
// manifest is the single source of truth for ordinals within the run.
//
// Legacy semantics (v2=false): the pool is the full session, so every cached
// message is present in the map; a miss keeps the message with whatever index
// it already had — the historical behavior, kept byte-identical.
//
// Returns a fresh kept slice and the number of dropped manifest-misses. The
// input may be backed by the process-wide message cache, so it must remain
// immutable after Retrieve releases the cache lock.
func assignCitationIndexes(messages []pipeline.Message, citationMap map[string]int, v2 bool) ([]pipeline.Message, int) {
	manifestMisses := 0
	kept := make([]pipeline.Message, 0, len(messages))
	for i := range messages {
		key := fmt.Sprintf("%s:%d", messages[i].ChannelID, messages[i].MessageSeq)
		if idx, found := citationMap[key]; found {
			msg := messages[i]
			msg.CitationIndex = idx
			kept = append(kept, msg)
			continue
		}
		if v2 {
			manifestMisses++
			continue
		}
		kept = append(kept, messages[i])
	}
	return kept, manifestMisses
}

// summarizeChunkFn is the Map call seam. Production always uses
// summarizeMessagesChunk; tests swap it to drive concurrency/order/error
// behaviour deterministically without a live LLM. It is only reassigned from
// tests, before any goroutine starts, so it needs no synchronisation.
var summarizeChunkFn = summarizeMessagesChunk

// chunkMapOutcome is one Map call's result, held in a slot indexed by chunk
// position. Every field is written by exactly one goroutine and read only after
// wg.Wait(), so no field needs synchronisation of its own.
type chunkMapOutcome struct {
	summary   string
	processed int
	oversized int
	err       error
}

// assembleMapOutput joins the kept chunk summaries into one Map document and,
// when coverageGap is set (any message was dropped — a failed or blank chunk, a
// manifest-miss, or the intentional fan-out cap), appends the V2-independent gap
// notice so the incompleteness is visible IN the text, not only in the run row.
//
// It appends the notice ONLY when there is real content to ship. An all-blank
// input (every entry whitespace — e.g. the caller handed through summaries that
// all came back empty) is a no-usable-Map result, returned as an error so
// classifyToolError makes it fatal-and-retryable and the planner can recover,
// rather than shipping a "summary" whose entire body is the notice (which would
// slip past the handle store's empty guard and reduce into a vacuous deliverable
// — #256 P1-R4). In production summarizeChunksConcurrently already drops blank
// chunks, so this guard is defense-in-depth. Pure + testable (#256 P2).
func assembleMapOutput(summaries []string, coverageGap bool) (string, error) {
	// Emptiness must be judged on the SUMMARIES, not the joined string: the
	// "\n\n---\n\n" separator is itself non-whitespace, so a TrimSpace on the
	// join would never see an all-blank set as empty.
	hasContent := false
	for _, s := range summaries {
		if strings.TrimSpace(s) != "" {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return "", fmt.Errorf("summarize_chunk: no usable Map output (every kept chunk failed, was dropped, or returned blank)")
	}
	combined := strings.Join(summaries, "\n\n---\n\n")
	if coverageGap {
		combined += mapCoverageGapNotice
	}
	return combined, nil
}

// capChunks bounds the summarize_chunk fan-out to maxChunkCalls, keeping the
// MOST RECENT chunks (the tail slice): the message pool is ascending by timestamp
// and the splitter preserves order, so the newest conversation — usually where
// the current decisions / action items live — is retained and the OLDER HEAD is
// dropped. Returns capped=true when truncation occurred so the caller discloses
// it (cov.ChunkCallsCapped + mapCoverageGapNotice). Pure + testable (#256 P2).
func capChunks(chunks [][]map[string]interface{}) (kept [][]map[string]interface{}, capped bool) {
	if len(chunks) <= maxChunkCalls {
		return chunks, false
	}
	return chunks[len(chunks)-maxChunkCalls:], true
}

// isFatalChunkError mirrors the worker's isFatalMapError
// (internal/worker/personal_processor.go): an output-truncation or
// reasoning-budget-exhaustion result is NOT a droppable transient — the model
// could not produce a complete chunk summary — so the whole Map phase aborts
// rather than silently omitting that slice. Keeps the agent path's tolerance
// class aligned with the worker's.
//
// ErrRequestTooLarge is fatal here too (#256 r9 P1): item 1's whole target is a
// single oversized message, which becomes its own chunk, and the guard fires
// from that chunk with a DETERMINISTIC error — retrying the same body can't
// shrink it. Tolerating it would swallow item 1's fatal REQUEST_TOO_LARGE
// classification (summarize_chunk is a criticalTool) and drop the chunk silently
// instead of failing fast, so item 2's tolerance must not apply to it.
func isFatalChunkError(err error) bool {
	return errors.Is(err, service.ErrOutputTruncated) ||
		errors.Is(err, service.ErrStreamOutputTruncated) ||
		errors.Is(err, service.ErrReasoningBudgetExhausted) ||
		errors.Is(err, service.ErrRequestTooLarge)
}

// summarizeChunkRecovered calls summarizeChunkFn and converts a panic below the
// Registry.Dispatch recovery boundary into an error, so the SERIAL path turns a
// panicking chunk into a tolerated per-chunk failure exactly like the concurrent
// goroutines (which recover in their own defer) instead of unwinding the tool.
func summarizeChunkRecovered(ctx context.Context, idx int, chunk []map[string]interface{}, specGuidance string) (summary string, processed, oversized int, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("map chunk %d panicked: %v", idx, p)
		}
	}()
	return summarizeChunkFn(ctx, chunk, specGuidance)
}

// mapPhaseFailedErr builds the error returned when any chunk in the invocation
// failed (recovery-first: the invocation owes a retry, contract §2). It keys %w — and therefore the Error() text classifyToolError
// reads — on the LOWEST-INDEX cause ALONE. classifyToolError matches on BOTH
// errors.Is and error-text substrings, so wrapping errors.Join (whose Is hits the
// UNION and whose text concatenates every cause) would let one non-transient
// cause anywhere in the set resolve the whole phase to the earliest / most
// pessimistic arm — turning a recoverable transient storm (a rate-limit / 5xx
// burst) into a fatal, non-retryable latched FAILED run (#256 round-7 P1-1). The
// lowest-index cause is deterministic (errs is built in index order on both
// paths); every cause is already logged per-chunk as it is dropped, so triage
// keeps them all — only classification narrows to one. Callers must ensure
// len(errs) > 0.
func mapPhaseFailedErr(totalChunks int, errs []error) error {
	if len(errs) > 1 {
		log.Printf("[summarize_chunk] %d chunk(s) failed with no usable summary; classifying on the lowest-index cause, %d further cause(s) logged above", len(errs), len(errs)-1)
	}
	return fmt.Errorf("summarize_chunk: no usable Map output from %d chunks: %w", totalChunks, errs[0])
}

// summarizeChunksConcurrently runs the Map phase over chunks with bounded
// concurrency and returns the kept summaries in ORIGINAL chunk order, aggregating
// coverage counters into cov. Concurrency 1 takes a dedicated serial path (see
// below) rather than a one-permit semaphore, so it is a true rollback switch.
//
// Why this is not a plain `go` over the old loop body:
//
//   - Order. Callers join the summaries into one document and the [n] citation
//     markers inside them index a frozen manifest, so a completion-ordered
//     append would reorder the deliverable. Results land in outcomes[i], never
//     appended, so output order is independent of completion order.
//   - Coverage counters. The serial loop incremented cov.ProcessedCount /
//     cov.OversizedMessageCount inside the loop body. Those are plain ints on a
//     shared struct; incrementing them from N goroutines is a data race and
//     would silently under-count coverage — the exact class of defect SS-01
//     exists to prevent. They are accumulated after Wait, in index order.
//   - Cancellation. Acquiring the semaphore selects on ctx.Done(): when the
//     request deadline fires, queued chunks abort immediately instead of
//     waiting for an in-flight LLM call to release a slot.
//   - Panic containment. Registry.Dispatch deliberately converts handler
//     panics into tool errors. Map workers run below that recovery boundary, so
//     each worker must recover locally to preserve the same process-safety
//     contract as the old serial loop.
//
// Failure policy — recovery-first (see docs/summarize-chunk-completeness-contract.md):
// a transient per-chunk failure (429/5xx/timeout/recovered panic) makes the whole
// invocation return an error, so the runner MarkMapFailed's it and the invocation
// owes a successful retry (a flag-independent, request-scoped gate) rather than
// shipping a partial. A single chunk's own per-attempt timeout is such a transient
// failure. A fatal per-chunk error (truncation / reasoning-budget exhaustion /
// ErrRequestTooLarge, see isFatalChunkError) and the RUN's own
// cancellation/expiry likewise abort the phase. The ONLY non-complete outcome that
// is shipped rather than retried is the intentional fan-out cap (handler level,
// disclosed). A chunk that SUCCEEDS but returns a blank summary is COVERED, not
// lost — the model found nothing worth reporting — so its messages count as
// processed and it is recorded only in cov.BlankChunkCount (contract §2–§4).
//
// Error reporting: the phase errors via mapPhaseFailedErr, which keys the wrap on
// the LOWEST-INDEX cause alone. It deliberately does NOT join all causes:
// classifyToolError matches on both errors.Is and error text, so a union would let
// one non-transient cause anywhere in the set drag the whole phase to the most
// pessimistic (fatal, non-retryable) arm. Every cause is already logged per-chunk,
// so triage keeps them; only classification keys on the single deterministic
// lowest-index cause (#256 round-7 P1-1).
func summarizeChunksConcurrently(ctx context.Context, chunks [][]map[string]interface{}, specGuidance string, cov *chunkCoverage) ([]string, error) {
	_, _, _, cfg := GetSummaryDeps()
	concurrency := cfg.ResolveAgentMapConcurrency()
	start := time.Now()

	// Concurrency 1 is the documented rollback setting, so it must reproduce the
	// pre-change loop EXACTLY — including dispatch order. A 1-permit semaphore
	// would still only run one call at a time, but the goroutines race for that
	// permit, so chunk 3 could be sent to the model before chunk 0. Output order
	// would survive (indexed slots), but "identical to before" would not: an
	// operator flipping this to 1 to debug a model-side ordering effect would
	// still not be running the old code path. Short-circuit instead.
	if concurrency <= 1 {
		summaries := make([]string, 0, len(chunks))
		var errs []error
		for i, chunk := range chunks {
			summary, processed, oversized, err := summarizeChunkRecovered(ctx, i, chunk, specGuidance)
			if err != nil {
				if ctx.Err() != nil {
					// The RUN itself is cancelled/expired — abort; continuing is pointless.
					return nil, fmt.Errorf("summarize chunk %d: %w", i, ctx.Err())
				}
				if isFatalChunkError(err) {
					// Truncation / reasoning-budget exhaustion is fatal, not a
					// droppable transient (mirrors worker isFatalMapError): abort.
					return nil, fmt.Errorf("summarize chunk %d: %w", i, err)
				}
				// Transient per-chunk failure, run still alive: record the cause.
				// Under the recovery-first contract (§2) ANY failure makes the phase
				// return an error below (the invocation owes a retry), keyed on the
				// lowest-index cause; nothing is shipped as a partial.
				log.Printf("[summarize_chunk] chunk %d failed; invocation owes a retry: %v", i, err)
				errs = append(errs, fmt.Errorf("chunk %d: %w", i, err))
				cov.FailedChunkCount++
				continue
			}
			if strings.TrimSpace(summary) == "" {
				// Blank-success: the model considered these messages and found nothing
				// worth reporting (the prompt tells it to skip off-topic chatter). They
				// are COVERED, not lost — count them as processed so a noisy-but-fully-
				// covered run does not assert incompleteness, and record BlankChunkCount
				// for observability only (contract §3/§4; #256 r9 blank P2).
				log.Printf("[summarize_chunk] chunk %d returned a blank summary; covered as noise, not appended", i)
				cov.BlankChunkCount++
				cov.ProcessedCount += processed
				cov.OversizedMessageCount += oversized
				continue
			}
			summaries = append(summaries, summary)
			cov.ProcessedCount += processed
			cov.OversizedMessageCount += oversized
		}
		if len(errs) > 0 {
			// Recovery-first (contract §2/§3): ANY transient per-chunk failure means
			// the invocation owes a retry — do not ship a partial. Returning an error
			// makes the runner MarkMapFailed (flag-independent), blocking Reduce and
			// the final answer until a successful retry or a fail-closed run. The
			// lowest-index cause drives classifyToolError.
			return nil, mapPhaseFailedErr(len(chunks), errs)
		}
		log.Printf("[summarize_chunk] map phase: chunks=%d concurrency=1 failed=%d elapsed=%dms",
			len(chunks), cov.FailedChunkCount, time.Since(start).Milliseconds())
		return summaries, nil
	}

	outcomes := make([]chunkMapOutcome, len(chunks))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, chunk := range chunks {
		wg.Add(1)
		go func(idx int, c []map[string]interface{}) {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					outcomes[idx] = chunkMapOutcome{err: fmt.Errorf("map chunk %d panicked: %v", idx, p)}
				}
			}()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				outcomes[idx] = chunkMapOutcome{err: ctx.Err()}
				return
			}
			defer func() { <-sem }()

			// Re-check after acquiring. select picks uniformly among READY cases,
			// so a goroutine can win the permit released by a call that just
			// aborted on cancellation — dispatching a fresh LLM request for a
			// request the client already gave up on. Without this guard a
			// cancelled run still burns up to `concurrency` extra completions.
			if err := ctx.Err(); err != nil {
				outcomes[idx] = chunkMapOutcome{err: err}
				return
			}

			summary, processed, oversized, err := summarizeChunkFn(ctx, c, specGuidance)
			outcomes[idx] = chunkMapOutcome{summary: summary, processed: processed, oversized: oversized, err: err}
		}(i, chunk)
	}
	wg.Wait()

	log.Printf("[summarize_chunk] map phase: chunks=%d concurrency=%d elapsed=%dms",
		len(chunks), concurrency, time.Since(start).Milliseconds())

	summaries := make([]string, 0, len(chunks))
	var errs []error
	for i, o := range outcomes {
		if o.err != nil {
			if ctx.Err() != nil {
				// The RUN's context is cancelled/expired — abort the whole phase.
				return nil, fmt.Errorf("summarize chunk %d: %w", i, ctx.Err())
			}
			if isFatalChunkError(o.err) {
				// Truncation / reasoning-budget exhaustion is fatal, not a
				// droppable transient (mirrors worker isFatalMapError): abort.
				return nil, fmt.Errorf("summarize chunk %d: %w", i, o.err)
			}
			// Transient per-chunk failure (e.g. this chunk's own upstream timeout),
			// run still alive: record the cause. Recovery-first (§2) — any failure
			// makes the phase return an error below (owes a retry), keyed on the
			// lowest-index cause; nothing ships as a partial.
			log.Printf("[summarize_chunk] chunk %d failed; invocation owes a retry: %v", i, o.err)
			errs = append(errs, fmt.Errorf("chunk %d: %w", i, o.err))
			cov.FailedChunkCount++
			continue
		}
		if strings.TrimSpace(o.summary) == "" {
			// Blank-success: covered as noise, not lost — count as processed and
			// record BlankChunkCount for observability only (contract §3/§4).
			log.Printf("[summarize_chunk] chunk %d returned a blank summary; covered as noise, not appended", i)
			cov.BlankChunkCount++
			cov.ProcessedCount += o.processed
			cov.OversizedMessageCount += o.oversized
			continue
		}
		summaries = append(summaries, o.summary)
		cov.ProcessedCount += o.processed
		cov.OversizedMessageCount += o.oversized
	}
	if len(errs) > 0 {
		// Recovery-first (contract §2/§3): any transient per-chunk failure means
		// the invocation owes a retry — do not ship a partial. The error makes the
		// runner MarkMapFailed (flag-independent); the lowest-index cause classifies.
		return nil, mapPhaseFailedErr(len(chunks), errs)
	}
	return summaries, nil
}
