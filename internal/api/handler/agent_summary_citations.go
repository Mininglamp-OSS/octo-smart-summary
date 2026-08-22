package handler

import (
	"context"
	"encoding/json"
	"errors"
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
	"gorm.io/gorm"
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
	return h.buildCitationsForSessionWithDB(ctx, h.db, sessionID, content, uid, requestID)
}

func (h *AgentSummaryHandler) buildCitationsForSessionWithDB(
	ctx context.Context,
	db *gorm.DB,
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
	err := db.WithContext(ctx).
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
		allMessages = overrideWithRunManifest(ctx, db, uid, sessionID, requestID, allMessages)
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
// time, or a replay), no manifest frozen by that run, or a DB error — it logs
// the reason, returns the input unchanged, and keeps the recomputed indexes.
// The fallback is always the full legacy recompute, never a partial override.
//
// The *gorm.DB is an explicit parameter, not h.db (#202): CreateAgentSummary
// calls buildCitationsForSessionWithDB(ctx, tx, ...) from inside the save
// transaction, so the manifest read must run on that same tx. Binding this to
// the handler's pooled h.db would read outside the transaction.
func overrideWithRunManifest(ctx context.Context, db *gorm.DB, uid, sessionID, requestID string, msgs []pipeline.Message) []pipeline.Message {
	if db == nil {
		return msgs
	}
	run, err := summaryrun.NewStore(db).GetByRequest(ctx, uid, sessionID, requestID)
	if err != nil {
		log.Printf("[citations] resolve run for frozen manifest failed session=%s request_id=%s: %v; falling back to legacy citation recompute", sessionID, requestID, err)
		return msgs
	}
	_, entries, found, err := artifact.NewStore(db).GetFrozenManifestByRun(ctx, uid, run.RunID)
	if err != nil {
		log.Printf("[citations] load frozen manifest failed session=%s run=%s: %v; falling back to legacy citation recompute", sessionID, run.RunID, err)
		return msgs
	}
	if !found {
		log.Printf("[citations] frozen manifest missing session=%s run=%s; falling back to legacy citation recompute", sessionID, run.RunID)
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
//
// A marker only counts when the content HAS citations to resolve against. The
// penalty for failing is a hard FAILED verdict (finishgate.Evaluate), which is
// far too heavy to hang on any bracketed integer in prose: `根据 [2024] 年规划`,
// `待办共 [3] 项`, and `GB/T [50011]` all failed a good summary. This repo
// already concedes such integers exist — stripUnresolvedCitationMarkers
// (agent_summary.go) strips only the markers belonging to a referenced artifact
// precisely because "unrelated bracketed integers remain user content".
//
// Rules, all narrowing:
//
//   - cits empty AND the run never fetched anything — keyed on the FACT
//     (coverage_measured=false), not on an expectation — the refine-borrowed
//     path: the rewritten text still carries the SOURCE summary's markers, and
//     nothing of its own can dangle → valid.
//   - cits empty BUT the run DID fetch → a build that produced zero citations yet
//     left a [1]-anchored citation sequence in the text is a failed/expired
//     citation build, not prose. Scoping the empty-slice exemption to the
//     never-fetched case is what stops such a build reporting COMPLETE with
//     [1][2][3] still in the content.
//
// The parameter is deliberately the fact and not run.FetchExpected. Keying it on
// the expectation reopened the exact class finishgate.Evaluate reordered itself
// to close: buildCitationsForSession runs on EVERY save and yields nil on error,
// while a soft rewrite persists fetch_expected=0 and KEEPS its fetch tools — so a
// soft rewrite that fetched, whose citation build then failed, passed validation
// with dangling markers and was labelled COMPLETE. An expectation may explain an
// absence; it may not overrule what the run actually did.
//   - a marker above the highest built index is prose, not a broken citation. A
//     run with 2 citations cannot have meant `[2024]`. Only an in-range miss —
//     e.g. [3] when 1, 2 and 4 exist — indicates real citation corruption.
func citationsValid(content string, cits []model.Citation, runFetchedAnything bool) bool {
	if len(cits) == 0 {
		if runFetchedAnything && contentHasCitationSequence(content) {
			return false
		}
		return true
	}
	idxSet := make(map[int]bool, len(cits))
	maxIdx := 0
	for _, c := range cits {
		idxSet[c.Index] = true
		if c.Index > maxIdx {
			maxIdx = c.Index
		}
	}
	for _, m := range citationMarkerRE.FindAllStringSubmatch(content, -1) {
		var n int
		if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil {
			continue
		}
		if n < 1 || n > maxIdx {
			// Outside the citation space entirely: a year, a count, a standard number.
			continue
		}
		if !idxSet[n] {
			return false
		}
	}
	return true
}

// contentHasCitationSequence reports whether content carries a [1] marker — the
// unambiguous start of a citation list. A real citation build always numbers from
// [1], whereas an incidental bracketed integer in prose (a year [2024], a count
// [3], a standard GB/T [50011]) does not produce a [1]. Used only to catch a
// fetch turn whose citation build produced zero citations yet left markers behind;
// keying on [1] keeps the prose-integer concession intact.
func contentHasCitationSequence(content string) bool {
	for _, m := range citationMarkerRE.FindAllStringSubmatch(content, -1) {
		if m[1] == "1" {
			return true
		}
	}
	return false
}

// finalizeRun computes the SS-07 finish verdict (COMPLETE/PARTIAL/FAILED) for
// the exact request that produced the saved content and persists it. It is
// best-effort and flag-gated by the caller. A missing/unknown request_id is a
// no-op: selecting "the latest run in the session" is unsafe because a late
// status update on an older run can move its updated_at past the generating run.
//
// Returns the verdict and gaps so the caller can surface them. A request with no
// run row returns ("", nil) and the save proceeds unchanged; only a genuine
// lookup FAILURE reports an unverified PARTIAL.
func (h *AgentSummaryHandler) finalizeRun(ctx context.Context, uid, sessionID, requestID, content string, cits []model.Citation) (finishgate.Verdict, []finishgate.Gap) {
	if h.db == nil || requestID == "" {
		return "", nil
	}
	runStore := summaryrun.NewStore(h.db)
	run, err := runStore.GetByRequest(ctx, uid, sessionID, requestID)
	if err != nil {
		// "No such run" and "could not read the run" are opposite situations and were
		// being reported identically.
		//
		// GetByRequest uses First(), so a request that simply has no run row comes
		// back as ErrRecordNotFound. That is the NORMAL path whenever V2 is off or
		// run creation was skipped/failed — there is nothing to judge, and reporting
		// PARTIAL there put an "incomplete" warning on perfectly ordinary saves.
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		// A genuine lookup failure is different: the run may well exist and be
		// incomplete, and we cannot tell. A disclosure feature must not degrade to
		// "no warning" — that is indistinguishable from a clean run. finalizeRun
		// executes AFTER the save transaction commits, so a client disconnecting the
		// instant the save returns cancels this ctx and lands here. Say we could not
		// verify.
		log.Printf("[finish] run lookup failed session=%s request=%s: %v", sessionID, requestID, err)
		return finishgate.Partial, []finishgate.Gap{{
			Kind:   finishgate.GapToolError,
			Detail: "could not verify run completeness",
		}}
	}

	state := finishgate.RunState{
		ScopeResolved:            run.SpecID != "",
		SummaryGenerated:         content != "",
		CitationValidationPassed: citationsValid(content, cits, run.CoverageMeasured),
		FetchExpected:            run.FetchExpected,
		CoverageMeasured:         run.CoverageMeasured,
		AttemptedChannels:        decodeFinishChannelIDs(run.AttemptedChannels),
		SucceededChannels:        decodeFinishChannelIDs(run.SucceededChannels),
		FailedChannels:           decodeFinishChannelIDs(run.FailedChannels),
		DiscoveredChannels:       decodeFinishChannelIDs(run.DiscoveredChannels),
		Truncated:                run.CoverageTruncated,
		OutputTruncated:          run.OutputTruncated,
		DroppedMessages:          run.DroppedMessages,
	}

	// SS-07b: a run marked failed by the tool-error hook (a fatal tool error
	// occurred mid-run) forces a FAILED verdict (defect #5).
	if run.Status == model.RunStatusFailed {
		state.CriticalToolErrors = append(state.CriticalToolErrors, "fatal tool error during run")
	}

	// Evidence facts from the frozen artifact (SS-04/05), when present.
	// Run-scoped read (issue A fix): a session can hold multiple runs (one per
	// submit/request_id), each with its own artifact. Reading by session would
	// grab a different run's artifact and mismatch this run's Spec channel count
	// → false PARTIAL. Key the artifact to the same run as the Spec below.
	artStore := artifact.NewStore(h.db)
	if art, ok, aerr := artStore.GetLatestArtifactByRun(ctx, uid, run.RunID); aerr == nil && ok {
		state.HasUsableEvidence = art.MessageCount > 0
	} else {
		// No artifact (e.g. legacy path): evidence existence is implied by cits.
		state.HasUsableEvidence = len(cits) > 0 || content != ""
	}
	if !state.HasUsableEvidence && run.CoverageMeasured && len(state.SucceededChannels) > 0 {
		// A successfully fetched quiet channel is still usable negative evidence:
		// the gate must not treat "0 messages" as "fetch never happened".
		state.HasUsableEvidence = true
	}

	// Expected channels from the run's Spec, when persisted.
	if spec, ok, serr := runStore.GetLatestSpec(ctx, uid, run.RunID); serr == nil && ok {
		state.ExpectedChannels = make([]string, 0, len(spec.Channels))
		for _, ch := range spec.Channels {
			if ch.ChannelID != "" {
				state.ExpectedChannels = append(state.ExpectedChannels, ch.ChannelID)
			}
		}
	}

	verdict, gaps := finishgate.Evaluate(state)
	if err := runStore.SetFinishStatus(ctx, uid, run.RunID, string(verdict)); err != nil {
		log.Printf("[finish] persist finish_status failed run=%s: %v", run.RunID, err)
	}
	log.Printf("[finish] session=%s run=%s verdict=%s gaps=%d", sessionID, run.RunID, verdict, len(gaps))
	return verdict, gaps
}

func decodeFinishChannelIDs(raw string) []string {
	if raw == "" {
		return nil
	}
	var channels []string
	if err := json.Unmarshal([]byte(raw), &channels); err != nil {
		return nil
	}
	return channels
}
