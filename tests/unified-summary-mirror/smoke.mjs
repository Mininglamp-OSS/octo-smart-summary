import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";
import { setTimeout } from "node:timers/promises";

// Real API + Worker entry points, real MySQL, real notification state machine.
// Only identity, the model response and octo-server's notify endpoint are synthetic.
const origin = "http://127.0.0.1:28371";
const space = "unified-fixture";
const headers = { token: "fixture-owner", "X-Space-Id": space, "Content-Type": "application/json" };

async function call(path, body, status = 200, override = {}) {
  const res = await fetch(origin + path, {
    headers: { ...headers, ...override },
    ...(body ? { method: "POST", body: JSON.stringify(body) } : {}),
  });
  const value = await res.json();
  assert.equal(res.status, status, `${path}: ${JSON.stringify(value)}`);
  return value.data;
}
function sql(statement) {
  // utf8mb4 explicitly: the fixtures are Chinese, and the client's default charset
  // would hand back '?' for every non-ASCII byte, quietly breaking content assertions.
  return execFileSync("docker", ["exec", "octo-summary-versioning-test-mysql", "mysql", "-uroot", "-N", "-B",
    "--default-character-set=utf8mb4", "summary_versioning_unified", "-e", statement], { encoding: "utf8" }).trim();
}
// The stub is on the isolated network; read its sink from inside the container.
function deliveries() {
  const raw = execFileSync("docker", ["compose", "-f", new URL("compose.yml", import.meta.url).pathname,
    "exec", "-T", "notifyfixture", "node", "-e",
    "fetch('http://127.0.0.1:8089/_notifications').then(r=>r.text()).then(t=>process.stdout.write(t))"],
    { encoding: "utf8" });
  return JSON.parse(raw.trim());
}

// --- 1. From-scratch agent save: no references, therefore no notification. ---
const scratch = await call("/api/v1/summaries/agent", {
  session_id: "sess-scratch",
  origin_channel_id: "group1",
  origin_channel_type: 1,
  title: "Alpha 项目周报",
  sources: [{ source_type: 1, source_id: "group1" }],
});
assert.ok(scratch.task_id > 0);
assert.equal(scratch.status, 3, "an agent save is born completed");
assert.equal(deliveries().length, 0, "a from-scratch agent summary must stay silent");
assert.equal(sql(`SELECT COUNT(*) FROM summary_notification WHERE task_id=${scratch.task_id}`), "0");

// --- 2. Continue-optimize: references the first summary, so it must notify. ---
// origin_channel_id is deliberately omitted: a refine session has no fetch_channel,
// so the new summary must inherit its origin from the referenced task.
const derived = await call("/api/v1/summaries/agent", {
  session_id: "sess-derived",
  title: "Alpha 项目周报(补充风险与下一步)",
  referenced_task_ids: [scratch.task_id],
});
assert.ok(derived.task_id > scratch.task_id, "continue-optimize creates a NEW summary");
assert.equal(sql(`SELECT origin_channel_id FROM summary_task WHERE id=${derived.task_id}`), "group1",
  "origin inherited from the referenced summary");
assert.equal(sql(`SELECT referenced_task_ids FROM summary_task WHERE id=${derived.task_id}`), `[${scratch.task_id}]`);
// The source summary is untouched: still present, still its own content. The two
// deliverables are deliberately different documents -- the source reports progress,
// the continue-optimize successor adds the risks and next steps its instruction asked
// for -- so "untouched" is checked against real text, not against a placeholder.
assert.equal(sql(`SELECT COUNT(*) FROM summary_task WHERE id=${scratch.task_id} AND deleted_at IS NULL`), "1");
const scratchBody = sql(`SELECT content FROM summary_personal_result WHERE task_id=${scratch.task_id}`);
const derivedBody = sql(`SELECT content FROM summary_personal_result WHERE task_id=${derived.task_id}`);
assert.match(scratchBody, /本周进展/);
assert.match(scratchBody, /压测达到 1200 QPS/);
assert.ok(!scratchBody.includes("风险与阻塞"), "the source must not have picked up the successor's new sections");
assert.match(derivedBody, /风险与阻塞/);
assert.match(derivedBody, /下一步/);
assert.ok(scratchBody.length > 200 && derivedBody.length > 200,
  `both deliverables must read as real documents, got ${scratchBody.length}/${derivedBody.length} chars`);

const derivedNo = sql(`SELECT task_no FROM summary_task WHERE id=${derived.task_id}`);
let sent = deliveries();
assert.equal(sent.length, 1, `exactly one delivery expected, got ${JSON.stringify(sent)}`);
assert.deepEqual(sent[0].targets, ["owner"]);
assert.equal(sent[0].space_id, space);
assert.equal(sent[0].card.kind, "completed");
assert.equal(sent[0].card.task_no, derivedNo);
assert.equal(sql(`SELECT status FROM summary_notification WHERE task_id=${derived.task_id}`), "sent");
assert.equal(sql(`SELECT COUNT(*) FROM summary_notification WHERE task_id=${scratch.task_id}`), "0",
  "the from-scratch save must not have produced a notification row either");

// --- 3. The derived agent summary starts with an incomplete configuration. ---
const base = `/api/v1/summaries/${derived.task_id}/contents`;
const current = async () => (await call(base)).contents[0];
let content = await current();
const prefix = `${base}/${content.content_id}`;
assert.equal(content.capabilities.can_configure_schedule, true, "配置入口 always open once completed");
assert.equal(content.capabilities.can_regenerate_with_config, true);
assert.equal(content.capabilities.can_schedule, false, "no schedule until the config is completed");
assert.equal(content.capabilities.can_regenerate_direct, false, "no direct regenerate until the config is completed");
let config = await call(`${prefix}/configuration`);
assert.equal(config.state, "incomplete", "an agent summary is born without a generation spec");

// --- 4. 补齐配置: the same summary becomes schedulable and directly regenerable. ---
const spec = {
  ...config.spec,
  requirement: "汇总 Alpha 项目本周进展、风险与决议",
  sources: [{ source_id: "group1", source_type: 1, confirmation: "user_confirmed" }],
};
const saved = await call(`${prefix}/configuration`, {
  expected_config_revision: config.revision,
  spec,
  schedule: { enabled: true, interval_days: 7, interval_months: 0, run_time: "09:00", day_of_week: 0, day_of_month: 0 },
});
assert.equal(saved.generation, null, "saving a configuration must not generate immediately");
assert.equal(saved.configuration.state, "complete");
content = await current();
assert.equal(content.capabilities.can_schedule, true, "定时更新 unlocked after 补齐配置");
assert.equal(content.capabilities.can_regenerate_direct, true, "直接重新生成 unlocked after 补齐配置");
assert.equal(content.current_version.version, 1, "configuring changed no content");

// --- 5. Direct regenerate produces a new version of the SAME summary. ---
const baseline = (c) => ({
  expected_current_version_id: c.current_version.version_id,
  expected_content_revision: c.content_revision,
});
async function finished(run) {
  for (let i = 0; i < 160; i++) {
    const state = await call(`${prefix}/generations/${run.generation_id}`);
    if (!["pending", "running"].includes(state.status)) return state;
    await setTimeout(250);
  }
  assert.fail("worker did not terminalize within 40 seconds");
}
const accepted = await call(`${prefix}/generations/regenerate`,
  { ...baseline(content), expected_config_revision: saved.configuration.revision, idempotency_key: randomUUID() }, 202);
assert.equal((await finished(accepted)).applied, true);
content = await current();
assert.equal(content.current_version.version, 2, "same summary, new version");
// A regenerated version is retrieved and summarised for real, so it must read as a
// document. The stub composes its answer from the topics present in the window, which
// is what makes V2 and V3 differ below rather than repeating one canned sentence.
const v2 = content.current_version.content ?? "";
assert.match(v2, /本周进展/, `regenerated content must be a real report, got ${JSON.stringify(v2.slice(0, 120))}`);
assert.ok(v2.length > 120, `regenerated content is too thin to test with: ${v2.length} chars`);
assert.equal(sql(`SELECT COUNT(*) FROM summary_task WHERE creator_id='owner' AND deleted_at IS NULL`), "2",
  "regeneration must not spawn another summary");

// --- 6. Scheduled update runs on the same summary through the real worker. ---
sql(`UPDATE summary_schedule s JOIN summary_task t ON t.schedule_id=s.id SET s.next_run_at=DATE_SUB(NOW(), INTERVAL 2 DAY) WHERE t.id=${derived.task_id}`);
let scheduled;
for (let i = 0; i < 160; i++) {
  const next = await current();
  if (next.latest_generation?.operation_type === "scheduled_generate") { scheduled = next.latest_generation; break; }
  await setTimeout(250);
}
assert.ok(scheduled, "real worker did not claim the due slot");
assert.equal((await finished(scheduled)).applied, true);
content = await current();
assert.equal(content.current_version.version, 3);
// The due slot sits two days back, so the scheduled window ends where the manual one
// began to trail off: the two versions must both be real reports AND differ, which is
// what makes 版本记录 worth opening in the UI.
const v3 = content.current_version.content ?? "";
assert.match(v3, /本周进展/, `scheduled content must be a real report, got ${JSON.stringify(v3.slice(0, 120))}`);
assert.notEqual(v3, v2, "a different window must produce a different version, not a repeat of V2");
assert.equal(sql(`SELECT COUNT(*) FROM summary_generation_run WHERE task_id=${derived.task_id} AND operation_type='scheduled_generate'`), "1");

// Regeneration and scheduling are content events, not new deliverables: the
// continue-optimize notification stays the only one.
sent = deliveries();
assert.equal(sent.length, 1, `continue-optimize must remain the only notification, got ${JSON.stringify(sent)}`);

console.log(`PASS: real API + Worker images; from-scratch save silent, continue-optimize notified (task ${derived.task_id}, card ${derivedNo}) without touching task ${scratch.task_id}, origin inherited, 补齐配置 unlocked schedule + direct regenerate, V2 (${v2.length} chars) manual and V3 (${v3.length} chars, different window) on the same summary`);
