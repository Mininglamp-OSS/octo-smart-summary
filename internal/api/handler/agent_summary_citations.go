package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/artifact"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/finishgate"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/worker"
)

// buildCitationsForSession 反查 session_id 的所有工具轨迹,组 messages 池,
// 调 worker.BuildCitations 得到结构化 Citation 数组。
// 若 content 里没有任何 [n] 标记,返回 []Citation{} (等价于 SetCitations(nil))。
//
// 实现策略:
//  1. 从 agent_message_evidence 提取本 (user_id, session_id) 的所有 handle
//  2. 每个 handle 优先走 agent.messageCache 恢复 messages (30分钟 TTL),
//     cache miss 时 fallback 到 evidence.Evidence 的 JSON snapshot
//  3. 合并去重 → 得到 allMessages 池
//  4. 为每条 message 分配 CitationIndex(1-indexed, 全局唯一, 时间升序)
//  5. 收集 nameMap: sender_uid -> sender_name
//  6. 调 worker.BuildCitations(content, allMessages, allMessages, nameMap)
//  7. 返回结果; 出错走 log + 返回空数组不阻塞落库(citations 是锦上添花不是必要)
//
// Discovery-source symmetry (#161 P1-A · yujiawei):
// Must discover handles from agent_message_evidence — byte-identical to
// getSessionMessagePool (internal/agent/tool_summarize_chunk.go). Previously
// this function discovered from agent_message WHERE role='tool' while
// getSessionMessagePool discovered from agent_message_evidence, so an
// orphan-evidence scenario (chat step fails before AppendMessages persists
// tool rows, but PersistEvidence already wrote its evidence row) produced
// a pool asymmetry: mid-run pool saw orphan rows, save-time pool did not,
// CitationIndex 1..N drifted between the two, [n] markers no longer lined
// up with saved Citation rows. Aligning both sites on evidence discovery
// closes that reachable failure path.
func (h *AgentSummaryHandler) buildCitationsForSession(
	ctx context.Context,
	sessionID string,
	content string,
	uid string,
	requestID string,
) ([]model.Citation, error) {
	// 1. Discover handles from agent_message_evidence — must stay symmetric
	// with getSessionMessagePool in tool_summarize_chunk.go. Rows are written
	// synchronously by PersistEvidence inside every data-fetching tool
	// (fetch_channel, peek_channel, search_messages, filter_relevant) before
	// the tool returns, so this discovery source is populated for every
	// handle the LLM could cite — regardless of whether the subsequent
	// AppendMessages persisted the corresponding agent_message tool row.
	var evidenceRows []model.AgentMessageEvidence
	err := h.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", uid, sessionID).
		Order("created_at ASC, handle ASC").
		Find(&evidenceRows).Error
	if err != nil {
		log.Printf("[citations] query evidence rows failed session=%s: %v", sessionID, err)
		return nil, err
	}

	if len(evidenceRows) == 0 {
		// No tool calls = no messages to cite
		return []model.Citation{}, nil
	}

	// 2. Resolve each handle to its messages: cache preferred (hot path),
	// evidence JSON snapshot as fallback (cold cache / restart).
	var allMessages []pipeline.Message
	seenKey := make(map[string]bool) // de-dup by channel_id+message_seq

	cache := agent.GetMessageCache()

	for _, ev := range evidenceRows {
		if ev.Handle == "" {
			continue
		}

		// Prefer cache (avoids JSON unmarshal on the hot path)
		if cached := cache.Retrieve(ev.Handle, uid); cached != nil {
			for _, msg := range cached {
				key := fmt.Sprintf("%s:%d", msg.ChannelID, msg.MessageSeq)
				if !seenKey[key] {
					allMessages = append(allMessages, msg)
					seenKey[key] = true
				}
			}
			log.Printf("[citations] retrieved %d messages from cache handle=%s", len(cached), ev.Handle)
			continue
		}

		// Cache miss: fallback to evidence JSON snapshot. Log both success
		// and unmarshal failure for parity with observability elsewhere.
		log.Printf("[citations] cache miss for handle=%s session=%s, falling back to evidence JSON", ev.Handle, sessionID)
		var evidenceMessages []pipeline.Message
		if err := json.Unmarshal([]byte(ev.Evidence), &evidenceMessages); err != nil {
			log.Printf("[citations] evidence unmarshal failed handle=%s: %v", ev.Handle, err)
			continue
		}
		for _, msg := range evidenceMessages {
			key := fmt.Sprintf("%s:%d", msg.ChannelID, msg.MessageSeq)
			if !seenKey[key] {
				allMessages = append(allMessages, msg)
				seenKey[key] = true
			}
		}
		log.Printf("[citations] retrieved %d messages from evidence table handle=%s", len(evidenceMessages), ev.Handle)
	}

	if len(allMessages) == 0 {
		// Tools were called but cache expired or no messages extracted
		log.Printf("[citations] no messages recovered session=%s (cache likely expired)", sessionID)
		return []model.Citation{}, nil
	}

	// 3. Sort by timestamp ascending, with (ChannelID, MessageSeq) as deterministic
	// tiebreaker. Must stay byte-identical to the sort in
	// internal/agent/tool_summarize_chunk.go:60-70 so that the pre-assigned
	// CitationIndex from the tool layer matches the post-assignment here —
	// see SUM-47 v3 rationale.
	sort.Slice(allMessages, func(i, j int) bool {
		if allMessages[i].Timestamp != allMessages[j].Timestamp {
			return allMessages[i].Timestamp < allMessages[j].Timestamp
		}
		if allMessages[i].ChannelID != allMessages[j].ChannelID {
			return allMessages[i].ChannelID < allMessages[j].ChannelID
		}
		return allMessages[i].MessageSeq < allMessages[j].MessageSeq
	})

	// 4. Assign CitationIndex (1-indexed, global sequential)
	for i := range allMessages {
		allMessages[i].CitationIndex = i + 1
	}

	// SS-05 B1: when V2 mode is on, prefer FROZEN manifest ordinals so a [n]
	// marker can't point at a different message than the model intended.
	//
	// The gate is keyed to THIS request, not to the flag alone: only a request
	// that carries the request_id of a run that actually froze a manifest may
	// adopt frozen ordinals. A request without request_id (any legacy client)
	// froze nothing and recomputed above — adopting an earlier run's manifest
	// here would renumber/drop its citations deterministically. No request_id
	// → keep the recomputed indexes (legacy path), never a partial override.
	if agent.SummaryV2Enabled() && requestID != "" {
		allMessages = h.overrideWithRunManifest(ctx, uid, sessionID, requestID, allMessages)
	}

	// 5. Build nameMap
	nameMap := make(map[string]string)
	for _, msg := range allMessages {
		if msg.SenderUID != "" && msg.SenderName != "" {
			nameMap[msg.SenderUID] = msg.SenderName
		}
	}

	// 6. Call worker.BuildCitations
	citations := worker.BuildCitations(content, allMessages, allMessages, nameMap)

	log.Printf("[citations] built %d citations from %d messages session=%s", len(citations), len(allMessages), sessionID)
	return citations, nil
}

// overrideWithRunManifest replaces recomputed CitationIndex values with the
// ordinals frozen by THIS request's run (SS-05 B1), so the save-time numbering
// matches what the mid-run summarize_chunk pass emitted. Messages not present
// in the frozen manifest were fetched after the freeze and are dropped from
// the citable set.
//
// On any miss — unknown request_id (the run never existed: V2 off at chat
// time, or a replay), no manifest frozen by that run, or a DB error — it
// returns the input unchanged and keeps the recomputed indexes. The fallback
// is always the full legacy recompute, never a partial override.
func (h *AgentSummaryHandler) overrideWithRunManifest(ctx context.Context, uid, sessionID, requestID string, msgs []pipeline.Message) []pipeline.Message {
	if h.db == nil {
		return msgs
	}
	run, err := summaryrun.NewStore(h.db).GetByRequest(ctx, uid, sessionID, requestID)
	if err != nil {
		return msgs
	}
	_, entries, found, err := artifact.NewStore(h.db).GetFrozenManifestByRun(ctx, uid, run.RunID)
	if err != nil || !found {
		return msgs
	}
	ord := artifact.OrdinalMap(entries)
	out := make([]pipeline.Message, 0, len(msgs))
	for _, m := range msgs {
		idx, ok := ord[fmt.Sprintf("%s:%d", m.ChannelID, m.MessageSeq)]
		if !ok {
			continue
		}
		m.CitationIndex = idx
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CitationIndex < out[j].CitationIndex })
	log.Printf("[citations] session=%s run=%s using frozen manifest ordinals (%d of %d messages citable)", sessionID, run.RunID, len(out), len(msgs))
	return out
}

var citationMarkerRE = regexp.MustCompile(`\[(\d+)\]`)

// citationsValid reports whether every [n] marker in content resolves to a built
// citation index. No markers → vacuously valid (nothing to break).
func citationsValid(content string, cits []model.Citation) bool {
	idxSet := make(map[int]bool, len(cits))
	for _, c := range cits {
		idxSet[c.Index] = true
	}
	for _, m := range citationMarkerRE.FindAllStringSubmatch(content, -1) {
		var n int
		if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil {
			continue
		}
		if !idxSet[n] {
			return false
		}
	}
	return true
}

// finalizeRun computes the SS-07 finish verdict (COMPLETE/PARTIAL/FAILED) for the
// session's run and persists it. Best-effort and flag-gated by the caller: for
// SS-07 (core) it records the verdict + gaps for disclosure; it does NOT yet
// block the save (that, plus the SSE done contract and runner-integrated retry,
// is SS-07b). Off / no run → no-op.
//
// Returns the verdict and gaps so the caller can surface them; on any lookup
// failure it returns ("", nil) and the save proceeds unchanged.
func (h *AgentSummaryHandler) finalizeRun(ctx context.Context, uid, sessionID, content string, cits []model.Citation) (finishgate.Verdict, []finishgate.Gap) {
	if h.db == nil {
		return "", nil
	}
	runStore := summaryrun.NewStore(h.db)
	run, found, err := runStore.GetLatestRunBySession(ctx, uid, sessionID)
	if err != nil || !found {
		return "", nil
	}

	state := finishgate.RunState{
		ScopeResolved:            run.SpecID != "",
		SummaryGenerated:         content != "",
		CitationValidationPassed: citationsValid(content, cits),
	}

	// SS-07b: a run marked failed by the tool-error hook (a fatal tool error
	// occurred mid-run) forces a FAILED verdict (defect #5).
	if run.Status == model.RunStatusFailed {
		state.CriticalToolErrors = append(state.CriticalToolErrors, "fatal tool error during run")
	}

	// Coverage facts from the frozen artifact (SS-04/05), when present.
	// Run-scoped read (issue A fix): a session can hold multiple runs (one per
	// submit/request_id), each with its own artifact. Reading by session would
	// grab a different run's artifact and mismatch this run's Spec channel count
	// → false PARTIAL. Key the artifact to the same run as the Spec below.
	artStore := artifact.NewStore(h.db)
	if art, ok, aerr := artStore.GetLatestArtifactByRun(ctx, uid, run.RunID); aerr == nil && ok {
		state.HasUsableEvidence = art.MessageCount > 0
		state.ChannelsFetched = art.ChannelCount
		state.Truncated = art.Truncated
		var failed []string
		if art.FailedChannels != "" {
			_ = json.Unmarshal([]byte(art.FailedChannels), &failed)
		}
		state.FailedChannels = failed
	} else {
		// No artifact (e.g. legacy path): evidence existence is implied by cits.
		state.HasUsableEvidence = len(cits) > 0 || content != ""
	}

	// Expected channels from the run's Spec, when persisted.
	if spec, ok, serr := runStore.GetLatestSpec(ctx, uid, run.RunID); serr == nil && ok {
		state.ChannelsExpected = len(spec.Channels)
	}

	verdict, gaps := finishgate.Evaluate(state)
	if err := runStore.SetFinishStatus(ctx, run.RunID, string(verdict)); err != nil {
		log.Printf("[finish] persist finish_status failed run=%s: %v", run.RunID, err)
	}
	log.Printf("[finish] session=%s run=%s verdict=%s gaps=%d", sessionID, run.RunID, verdict, len(gaps))
	return verdict, gaps
}
