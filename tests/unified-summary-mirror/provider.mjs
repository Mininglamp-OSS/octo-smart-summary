import http from "node:http";

// Synthetic identities, model responses and notification sink. Bound only to the
// isolated fixture network; never forwards prompts, credentials or notifications
// externally. This fixture must never be used as a production service.
const NOTIFY_TOKEN = "local-synthetic-notify-token";
const notifications = [];

// The seeded conversation (see seed.sql) is spread across a week, and each message
// carries one of these keywords. Building the reply out of whichever keywords appear
// in the prompt keeps the stub deterministic — the same window always yields the same
// text, so map/reduce calls within one generation stay consistent — while making a
// version's content actually reflect the window it summarised. A manual regenerate
// ([now-7d, now]) and a scheduled run whose slot sits two days back therefore produce
// visibly different reports instead of the same canned sentence.
const TOPICS = [
  ["联调", "支付回调接口与结算侧联调完成,以幂等键去重,重复回调不再产生双笔"],
  ["压测", "压测达到 1200 QPS,P99 延迟 380ms;瓶颈在数据库连接池,max_open 已调整到 64"],
  ["灰度", "灰度按批次放量,从 5%(错误率 0.2%)推进到 20%,指标平稳无新增告警"],
  ["埋点", "下单漏斗三个节点的埋点补齐,数据看板第一版即将产出"],
  ["文档", "对外文档补充错误码表与限流说明,待下周一评审"],
  ["监控", "监控告警阈值重新校准,误报由每天 30 条降至 3 条"],
];
const RISKS = [
  ["对账", "对账历史数据迁移未完成,依赖 DBA 排期,是当前最大的阻塞"],
  ["回滚", "一次配置回滚暴露了发布前校验缺口,启动校验已补,同类配置项仍需全量盘查"],
  ["导出", "试点客户反馈导出偏慢,根因是全表扫描,索引方案上线后需复核 P99"],
];
const DECISIONS = [["发布延到", "Alpha 正式发布延至下周二,先完成对账迁移"]];

function report(prompt) {
  const hit = (pairs) => pairs.filter(([key]) => prompt.includes(key)).map(([, line]) => line);
  const progress = hit(TOPICS);
  const risks = hit(RISKS);
  const decisions = hit(DECISIONS);
  if (progress.length === 0 && risks.length === 0) {
    return "## Alpha 项目周报\n\n本次时间范围内没有检索到与主题相关的讨论。";
  }
  const sections = ["## Alpha 项目周报"];
  // Citation markers are numbered across the whole document, not per section: the
  // real pipeline drops a marker it has already seen, which would leave the later
  // sections with dangling references.
  let cite = 0;
  const bullets = (lines) => lines.map((line) => `- ${line} [${++cite}]`).join("\n");
  if (progress.length) sections.push(`### 本周进展\n\n${bullets(progress)}`);
  if (risks.length) sections.push(`### 风险与阻塞\n\n${bullets(risks)}`);
  if (decisions.length) sections.push(`### 决议\n\n${bullets(decisions)}`);
  sections.push(`### 覆盖范围\n\n本次共归纳 ${cite} 个要点。`);
  return sections.join("\n\n");
}

// ── The agent route ──────────────────────────────────────────────────────────
// The summary_workspace agent profile refuses a free-text final answer: every turn
// has to end through the terminal tool `emit_summary_response`, and the handler
// pins which result_type this turn is allowed to use inside the system prompt. A
// stub that only ever returns prose therefore fails every agent turn with a 500,
// which would leave 继续优化 (reference the current summary -> agent conversation ->
// save a NEW summary) untestable in this fixture even though the rest of the
// pipeline is real. So mirror the tool protocol: same report body as the
// non-agent path, wrapped in the terminal tool call the profile demands.
const TERMINAL_TOOL = "emit_summary_response";

function terminalArguments(prompt) {
  // The route is whatever the handler asked for, not a guess: it writes
  // "result_type=agent_preview" / "=agent_revision" / "=explanation" into the
  // prompt, and any other value is rejected by the terminal tool.
  if (prompt.includes("result_type=explanation")) {
    return { result_type: "explanation", reply: "这条总结的时间范围、来源聊天和引用都来自上面的预览，没有改动。" };
  }
  const revision = prompt.includes("result_type=agent_revision");
  const content = revision
    ? `${report(prompt)}\n\n### 本轮修订说明\n\n已按最新指令重写全文，引用编号与覆盖范围同步更新。`
    : report(prompt);
  return {
    result_type: revision ? "agent_revision" : "agent_preview",
    execution_target: "agent_preview",
    reply: revision ? "预览已按你的要求改写，可以继续调整或保存。" : "预览已生成，可以继续调整或直接保存。",
    // The server overwrites the version with the session's next one; 1 only has to
    // satisfy the tool schema's `required`.
    preview: { content, version: 1 },
  };
}

http.createServer(async (request, response) => {
  // Read-only introspection of what the API delivered. GET so the smoke test can
  // observe deliveries without mutating the sink.
  if (request.method === "GET" && request.url === "/_notifications") {
    response.writeHead(200, { "content-type": "application/json" });
    response.end(JSON.stringify(notifications));
    return;
  }
  if (request.method !== "POST") { response.writeHead(404).end(); return; }
  let raw = "";
  for await (const chunk of request) {
    raw += chunk;
    if (raw.length > 2_000_000) { response.writeHead(413).end(); return; }
  }
  let body;
  try { body = JSON.parse(raw); } catch { response.writeHead(400).end(); return; }

  if (request.url === "/v1/auth/verify") {
    const uid = { "fixture-owner": "owner", "fixture-outsider": "outsider" }[body.token];
    if (!uid) { response.writeHead(401).end(); return; }
    response.writeHead(200, { "content-type": "application/json" });
    response.end(JSON.stringify({ uid, name: uid }));
    return;
  }

  // octo-server's internal notify endpoint. The real server rejects a wrong
  // X-Internal-Token, and returns 200 with a `delivered` list that the client
  // treats as the only proof of delivery — mirror both so the API's own
  // delivery bookkeeping is exercised, not bypassed.
  if (request.url === "/v1/internal/notify") {
    if (request.headers["x-internal-token"] !== NOTIFY_TOKEN) {
      response.writeHead(401, { "content-type": "application/json" });
      response.end(JSON.stringify({ delivered: [], filtered: {} }));
      return;
    }
    const targets = Array.isArray(body.targets) ? body.targets : [];
    notifications.push({ at: Date.now(), space_id: body.space_id, service: body.service, targets, card: body.card ?? null });
    response.writeHead(200, { "content-type": "application/json" });
    response.end(JSON.stringify({ delivered: targets, filtered: {} }));
    return;
  }

  if (request.url !== "/v1/chat/completions") { response.writeHead(404).end(); return; }
  const prompt = JSON.stringify(body.messages);
  if (prompt.includes("fixture:fail")) { response.writeHead(400).end("synthetic model failure"); return; }
  const wantsTerminalTool = (body.tools ?? []).some((tool) => tool?.function?.name === TERMINAL_TOOL);
  if (wantsTerminalTool) {
    response.writeHead(200, { "content-type": "application/json" });
    response.end(JSON.stringify({
      choices: [{
        message: {
          role: "assistant",
          content: "",
          tool_calls: [{
            id: "call_fixture_terminal",
            type: "function",
            function: { name: TERMINAL_TOOL, arguments: JSON.stringify(terminalArguments(prompt)) },
          }],
        },
        finish_reason: "tool_calls",
      }],
      usage: { total_tokens: 10 },
    }));
    return;
  }
  const content = report(prompt);
  if (body.stream) {
    response.writeHead(200, { "content-type": "text/event-stream" });
    response.write(`data: ${JSON.stringify({ choices: [{ delta: { content }, finish_reason: null }] })}\n\n`);
    response.end('data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"total_tokens":10}}\n\ndata: [DONE]\n\n');
  } else {
    response.writeHead(200, { "content-type": "application/json" });
    response.end(JSON.stringify({ choices: [{ message: { role: "assistant", content }, finish_reason: "stop" }], usage: { total_tokens: 10 } }));
  }
}).listen(8089, "0.0.0.0");
