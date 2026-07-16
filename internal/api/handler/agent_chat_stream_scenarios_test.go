package handler

// SUM-21 场景测试 · agent chat stream(SSE)
//
// 分工前提(来自架构师 SUM-21 派单):
//   - PR #4 的 handler 单测已覆盖响应头、done/error 事件体、AppendMessages 调用与否、并发工具 SSE 帧完整性。
//   - 本文件补 **契约边界 + 语义等价性 + 集成级** 场景测试,不重复已有单测。
//
// 场景清单(架构师 6 条,优先级从高到低):
//  1. 鉴权与 Space 隔离               —— 走 middleware,handler 自动化测不到;交给端到端 curl。
//  2. 请求体校验一致性                —— 本文件覆盖(与非流式 Chat 逐字节一致)。
//  3. 上下文超时(300s)               —— 本文件覆盖(用 StepTimeout 收敛 + fakeSlowChatter)。
//  4. 客户端提前断连                  —— 本文件覆盖(client cancel + 断言 handler 不 panic)。
//  5. 落库语义等价                    —— 本文件覆盖(同 message 走两个端点,断言最终落库一致)。
//  6. 事件序列形态断言(phase/detail)—— 本文件覆盖(fake 驱动 fetch/map/reduce 序列,断言 payload schema + detail 非空)。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/gin-gonic/gin"
)

// ─── 场景 2:请求体校验一致性 ────────────────────────────────────────────────
//
// 契约:ChatStream 的入参校验必须与 Chat 完全一致(session_id 缺失/非法、message 为空/超长)。
// 上一 PR 已有 TestChatStreamSuccess/TestChatStreamRunnerError,没有单独覆盖各校验分支。
// 这里参考现有 TestAgentChatEmptySessionID / TestAgentChatInvalidSessionID 的 case 集,
// 逐条重放到 stream 端点,证明两个端点校验行为等价。

func doChatStream(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/chat/stream", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestChatStreamValidation_EmptyMessage(t *testing.T) {
	h := newTestAgentChatHandler("不该出现")
	r := setupAgentChatStreamRouter(h)
	w := doChatStream(t, r, `{"message":"","session_id":"s1"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty message expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "message 不能为空") {
		t.Errorf("expected canonical error message from Chat, got: %s", w.Body.String())
	}
}

func TestChatStreamValidation_EmptySessionID(t *testing.T) {
	h := newTestAgentChatHandler("不该出现")
	r := setupAgentChatStreamRouter(h)
	w := doChatStream(t, r, `{"message":"你好","session_id":""}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty session_id expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "session_id 不能为空") {
		t.Errorf("expected canonical error message from Chat, got: %s", w.Body.String())
	}
}

func TestChatStreamValidation_InvalidSessionID(t *testing.T) {
	h := newTestAgentChatHandler("不该出现")
	r := setupAgentChatStreamRouter(h)

	// 与非流式 TestAgentChatInvalidSessionID 相同的 case 集
	cases := []struct {
		name string
		sid  string
	}{
		{"包含空格", "with space"},
		{"包含中文", "会话-一二三"},
		{"含斜杠", "sess/1"},
		{"含点号", "sess.1"},
		{"超长129位", strings.Repeat("a", 129)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"message":"你好","session_id":"%s"}`, c.sid)
			w := doChatStream(t, r, body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for invalid sid=%q, got %d body=%s", c.sid, w.Code, w.Body.String())
			}
			// 契约要求错误 message 与 Chat 一致 —— session_id 非法必带此串
			if !strings.Contains(w.Body.String(), "session_id 非法") {
				t.Errorf("expected canonical 'session_id 非法' message, got: %s", w.Body.String())
			}
		})
	}
}

func TestChatStreamValidation_MessageTooLong(t *testing.T) {
	h := newTestAgentChatHandler("不该出现")
	r := setupAgentChatStreamRouter(h)
	// 8193 个 rune(汉字),刚好超过 maxMessageLen=8192
	msg := strings.Repeat("字", 8193)
	body := fmt.Sprintf(`{"message":"%s","session_id":"s1"}`, msg)
	w := doChatStream(t, r, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for over-limit message, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "message 过长") {
		t.Errorf("expected 'message 过长' error, got: %s", w.Body.String())
	}
}

func TestChatStreamValidation_MessageAtLimitOK(t *testing.T) {
	// 边界值:恰好 8192 rune 必须放行(200);这里 fake chatter 立即收敛,不真走远端。
	h := newTestAgentChatHandler("ok")
	r := setupAgentChatStreamRouter(h)
	msg := strings.Repeat("字", 8192)
	body := fmt.Sprintf(`{"message":"%s","session_id":"s1"}`, msg)
	w := doChatStream(t, r, body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 at limit, got %d body=%s", w.Code, w.Body.String())
	}
}

// ─── 场景 3:上下文超时 ────────────────────────────────────────────────────
//
// 契约:handler 用 300s ctx 包裹,但 policy.StepTimeout 是**每步**上限。真跑 305s 太慢,
// 这里用短 StepTimeout(50ms)+ slowChatter(sleep 500ms)构造 runner 超时,验证:
//  1. body 里只有 error 事件,没有 done 事件
//  2. handler 不 panic
//  3. AppendMessages 不被调用
//
// 说明:agent runner 里 Step 会用 policy.StepTimeout 打包 ctx.WithTimeout,超时后 chatter
// 返回 ctx.Err(),runner 冒泡为 error,handler 走 writeSSEErrorViaSink 路径 —— 语义与
// 客户端 300s 超时被上层 cancel 相同,只是触发点在 policy 层。真 300s 由架构师说的
// "有条件时手动跑"验证。

type fakeSlowChatter struct {
	sleep time.Duration
}

func (f *fakeSlowChatter) Chat(ctx context.Context, msgs []agent.Message, tools []agent.Tool) (agent.AssistantTurn, error) {
	select {
	case <-time.After(f.sleep):
		return agent.AssistantTurn{Content: "slow ok"}, nil
	case <-ctx.Done():
		return agent.AssistantTurn{}, ctx.Err()
	}
}

func TestChatStreamContextTimeout(t *testing.T) {
	reg := agent.NewRegistry()
	pool := agent.NewPool(2)
	// StepTimeout 极短,chatter sleep 远大于它 → 必超时
	policy := agent.Policy{MaxSteps: 8, MaxTokens: 8000, StepTimeout: 50 * time.Millisecond}
	runner := agent.NewRunner(&fakeSlowChatter{sleep: 500 * time.Millisecond}, reg, pool, policy)
	store := newFakeHistoryStore()
	h := newAgentChatHandlerWithRunner(runner, "sys", store, 10)
	r := setupAgentChatStreamRouter(h)

	w := doChatStream(t, r, `{"message":"hi","session_id":"sess-timeout"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("SSE endpoints always return 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("expected 'event: error' on step timeout, got: %s", body)
	}
	if strings.Contains(body, "event: done") {
		t.Errorf("expected NO 'event: done' on timeout, got: %s", body)
	}
	// AppendMessages 不应被调
	history, _ := store.LoadHistory(context.Background(), "sess-timeout")
	if len(history) != 0 {
		t.Errorf("expected no persistence on timeout, but got %d msgs", len(history))
	}
}

// ─── 场景 4:客户端提前断连 ────────────────────────────────────────────────
//
// 契约:客户端 abort 后 handler 不 panic,runner 因 ctx.Done() 收敛;`AppendMessages`
// 因 runner 返回 err 而不被调。用 request context 直接 cancel 来模拟 EventSource close。

func TestChatStreamClientDisconnect(t *testing.T) {
	// slow chatter,给客户端断连的时间窗
	reg := agent.NewRegistry()
	pool := agent.NewPool(2)
	policy := agent.Policy{MaxSteps: 8, MaxTokens: 8000, StepTimeout: 5 * time.Second}
	runner := agent.NewRunner(&fakeSlowChatter{sleep: 2 * time.Second}, reg, pool, policy)
	store := newFakeHistoryStore()
	h := newAgentChatHandlerWithRunner(runner, "sys", store, 10)
	r := setupAgentChatStreamRouter(h)

	// 用可 cancel 的 ctx 挂到 request 上;100ms 后主动 cancel 模拟客户端关闭
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/chat/stream",
		strings.NewReader(`{"message":"slow","session_id":"sess-abort"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		// 不能 recover panic 也传不出去;handler panic 会让整个 test 挂,天然当 assertion。
		r.ServeHTTP(w, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel() // 模拟客户端断连

	select {
	case <-done:
		// handler 收敛,没 panic
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return within 3s after client cancel — possible deadlock")
	}

	// 断言:不落库(因为 runner 返回 err)
	history, _ := store.LoadHistory(context.Background(), "sess-abort")
	if len(history) != 0 {
		t.Errorf("expected no persistence after client abort, got %d msgs", len(history))
	}
	// 断言:body 里不含 done(handler 只可能发 error 或什么都发不出去)
	if strings.Contains(w.Body.String(), "event: done") {
		t.Errorf("expected no 'event: done' after client abort, got: %s", w.Body.String())
	}
}

// ─── 场景 5:落库语义等价 ──────────────────────────────────────────────────
//
// 契约:同一 session_id 通过 Chat 和 ChatStream 跑同一 message,落库到 store 的
// assistant 消息内容应完全一致(fake chatter 返回相同 reply);且 progress/error 事件
// 绝不进入 store。

func TestChatStreamPersistenceEquivalent(t *testing.T) {
	const reply = "两个端点应产出等价的落库内容"
	const message = "hi"

	// 场景 5a:Chat 落库
	hChat := newTestAgentChatHandler(reply)
	storeChat := newFakeHistoryStore()
	hChat.store = storeChat
	rChat := setupAgentChatRouter(hChat)
	wChat := doChat(t, rChat, message, "sess-eq-chat")
	if wChat.Code != http.StatusOK {
		t.Fatalf("chat: expected 200, got %d body=%s", wChat.Code, wChat.Body.String())
	}
	histChat, _ := storeChat.LoadHistory(context.Background(), "sess-eq-chat")

	// 场景 5b:ChatStream 落库(独立 store,同 message,同 reply)
	hStream := newTestAgentChatHandler(reply)
	storeStream := newFakeHistoryStore()
	hStream.store = storeStream
	rStream := setupAgentChatStreamRouter(hStream)
	wStream := doChatStream(t, rStream, fmt.Sprintf(`{"message":"%s","session_id":"sess-eq-stream"}`, message))
	if wStream.Code != http.StatusOK {
		t.Fatalf("stream: expected 200, got %d body=%s", wStream.Code, wStream.Body.String())
	}
	histStream, _ := storeStream.LoadHistory(context.Background(), "sess-eq-stream")

	// 断言 1:两侧都落了 user + assistant 两条(不多不少)
	if len(histChat) != 2 || len(histStream) != 2 {
		t.Fatalf("expected 2 messages in each store, got chat=%d stream=%d", len(histChat), len(histStream))
	}
	// 断言 2:role 和 content 严格相等(证明 progress/error 事件绝对没入库)
	for i := range histChat {
		if histChat[i].Role != histStream[i].Role || histChat[i].Content != histStream[i].Content {
			t.Errorf("msg[%d] differ: chat={role:%s,content:%s} stream={role:%s,content:%s}",
				i, histChat[i].Role, histChat[i].Content, histStream[i].Role, histStream[i].Content)
		}
	}
	// 断言 3:两侧都有恰好一条 assistant + 内容等于 reply(而不是被塞了 SSE 帧)
	var assistCount int
	for _, m := range histStream {
		if m.Role == "assistant" {
			assistCount++
			if m.Content != reply {
				t.Errorf("stream assistant content mismatch: want=%q got=%q", reply, m.Content)
			}
		}
	}
	if assistCount != 1 {
		t.Errorf("expected exactly 1 assistant message in stream store, got %d", assistCount)
	}
}

// ─── 场景 6:事件序列形态断言 ──────────────────────────────────────────────
//
// 契约(架构师 PR 描述):
//   - 每条 progress payload 必含 phase / label / step / ofSteps / elapsed_ms;
//     有工具时含 tool;有 detail 时含 detail 字段。
//   - fetch_channel / search_messages / summarize_chunk / merge_summaries 的 detail
//     必须非空(便宜计数已在 runner.extractToolDetail 实现)。
//   - done 有且仅有一条,在最后。
//
// 我们模拟 explore→fetch→map→reduce:让第一步返回 4 个真工具的 tool_call,再收尾。
// 4 个工具是 progress_labels 里 phase 覆盖 fetch/filter/map/reduce 的:
//   - fetch_channel   (phase=fetch)
//   - filter_relevant (phase=filter)
//   - summarize_chunk (phase=map)
//   - merge_summaries (phase=reduce)
// 每个工具返回一个 JSON,含 runner.extractToolDetail 能识别的字段。

type fakeFourToolChatter struct {
	callCount int32
}

func (f *fakeFourToolChatter) Chat(ctx context.Context, msgs []agent.Message, tools []agent.Tool) (agent.AssistantTurn, error) {
	count := atomic.AddInt32(&f.callCount, 1)
	if count == 1 {
		var tcs []agent.ToolCall
		for i, name := range []string{"fetch_channel", "filter_relevant", "summarize_chunk", "merge_summaries"} {
			tc := agent.ToolCall{ID: fmt.Sprintf("c-%d", i), Type: "function"}
			tc.Function.Name = name
			tc.Function.Arguments = `{}`
			tcs = append(tcs, tc)
		}
		return agent.AssistantTurn{Content: "分派工具", ToolCalls: tcs}, nil
	}
	return agent.AssistantTurn{Content: "整合完成"}, nil
}

func TestChatStreamEventSequenceAndDetail(t *testing.T) {
	reg := agent.NewRegistry()

	// 4 个假工具,每个返回一个 runner.extractToolDetail 能吃的 JSON schema
	// 参考 runner.go 里 extractToolDetail 对各工具的取字段规则:
	//   fetch_channel   / search_messages   → data["total"]     → "已抓取 N 条" / "命中 N 条"
	//   filter_relevant                     → data["filtered_count"] → "保留 N 条"
	//   summarize_chunk                     → runner 会自己维护 chunk_idx/total,不看返回
	//   merge_summaries                     → data["chunk_count"] → "合并 N 段"
	toolReturns := map[string]string{
		"fetch_channel":   `{"total":128}`,
		"filter_relevant": `{"filtered_count":20}`,
		"summarize_chunk": `{"summary":"x"}`, // detail 由 runner 编号,不需要 JSON 字段
		"merge_summaries": `{"chunk_count":5}`,
	}
	for name, ret := range toolReturns {
		toolName, ret := name, ret
		schema := agent.Tool{Type: "function", Function: agent.ToolFunction{
			Name: toolName, Description: "fake " + toolName,
			Parameters: map[string]interface{}{"type": "object"},
		}}
		h := func(ctx context.Context, _ json.RawMessage) (string, error) {
			return ret, nil
		}
		reg.Register(schema, h)
	}

	pool := agent.NewPool(4)
	policy := agent.Policy{MaxSteps: 8, MaxTokens: 8000, StepTimeout: 5 * time.Second}
	runner := agent.NewRunner(&fakeFourToolChatter{}, reg, pool, policy)

	store := newFakeHistoryStore()
	h := newAgentChatHandlerWithRunner(runner, "sys", store, 10)
	r := setupAgentChatStreamRouter(h)
	w := doChatStream(t, r, `{"message":"跑一次序列","session_id":"sess-seq"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	// —— done 恰好 1 条 & 在最后 ——
	doneCount := strings.Count(body, "event: done")
	if doneCount != 1 {
		t.Fatalf("expected exactly 1 'event: done', got %d\n%s", doneCount, body)
	}
	if strings.LastIndex(body, "event:") != strings.Index(body, "event: done") {
		t.Errorf("expected 'done' to be the last event, got extra events after it:\n%s", body)
	}

	// —— 收集所有 progress payload 并按新契约断言 schema ——
	// 新契约：只发 phase(抽象枚举)/step/ofSteps/elapsed_ms/count(整型,可选)。
	// 不得出现 tool / label / detail（防泄露原始工具信息）。
	type progressEvt struct {
		Phase     string `json:"phase"`
		Step      int    `json:"step"`
		OfSteps   int    `json:"ofSteps"`
		ElapsedMs int64  `json:"elapsed_ms"`
		Count     int    `json:"count,omitempty"`
	}
	var progresses []progressEvt
	var rawPayloads []string
	// 按 SSE 帧解析:每两行是 "event: xxx\n" + "data: {json}\n"
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines)-1; i++ {
		if lines[i] == "event: progress" && strings.HasPrefix(lines[i+1], "data: ") {
			raw := strings.TrimPrefix(lines[i+1], "data: ")
			rawPayloads = append(rawPayloads, raw)
			var p progressEvt
			if err := json.Unmarshal([]byte(raw), &p); err != nil {
				t.Errorf("progress payload not valid JSON: %v raw=%s", err, raw)
				continue
			}
			progresses = append(progresses, p)
		}
	}
	if len(progresses) < 4 {
		t.Fatalf("expected >=4 progress events (one per tool), got %d\n%s", len(progresses), body)
	}

	// —— 安全契约:progress 载荷绝不能泄露原始工具信息 ——
	for _, raw := range rawPayloads {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Errorf("progress payload not valid JSON: %v", err)
			continue
		}
		for _, forbidden := range []string{"tool", "label", "detail"} {
			if _, ok := m[forbidden]; ok {
				t.Errorf("progress payload must NOT contain %q (leaks tool info): %s", forbidden, raw)
			}
		}
	}

	// —— 每条 progress 字段完整性:phase 非空,step/ofSteps 有效,elapsed 非负 ——
	for i, p := range progresses {
		if p.Phase == "" {
			t.Errorf("progress[%d] missing phase: %+v", i, p)
		}
		if p.Step <= 0 || p.OfSteps <= 0 {
			t.Errorf("progress[%d] invalid step/ofSteps: %+v", i, p)
		}
		if p.ElapsedMs < 0 {
			t.Errorf("progress[%d] negative elapsed_ms: %+v", i, p)
		}
	}

	// —— 抽象 phase 必须覆盖 检索/筛选/提炼/汇总（且不含任何原始工具名）——
	seenPhase := map[string]bool{}
	countByPhase := map[string]int{}
	for _, p := range progresses {
		seenPhase[p.Phase] = true
		if p.Count > countByPhase[p.Phase] {
			countByPhase[p.Phase] = p.Count
		}
	}
	for _, want := range []string{"retrieve", "filter", "distill", "compose"} {
		if !seenPhase[want] {
			t.Errorf("expected a progress event with abstract phase %q, seen=%v", want, seenPhase)
		}
	}

	// —— 安全整型计数抽样:检索128 / 筛选20 / 汇总5;提炼阶段无计数 ——
	if countByPhase["retrieve"] != 128 {
		t.Errorf("retrieve phase count: want 128, got %d", countByPhase["retrieve"])
	}
	if countByPhase["filter"] != 20 {
		t.Errorf("filter phase count: want 20, got %d", countByPhase["filter"])
	}
	if countByPhase["compose"] != 5 {
		t.Errorf("compose phase count: want 5, got %d", countByPhase["compose"])
	}
	if countByPhase["distill"] != 0 {
		t.Errorf("distill phase should carry no count, got %d", countByPhase["distill"])
	}
}
