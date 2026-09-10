import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";
import { setTimeout } from "node:timers/promises";

const base = "http://127.0.0.1:28351/api/v1/summaries/110/contents";
const headers = { token: "fixture-owner", "X-Space-Id": "execution-fixture", "Content-Type": "application/json" };
async function request(path = "", body, status = 200, override = {}) {
  const res = await fetch(base + path, { headers: { ...headers, ...override }, ...(body ? { method: "POST", body: JSON.stringify(body) } : {}) });
  const value = await res.json();
  assert.equal(res.status, status, `${path}: ${JSON.stringify(value)}`);
  return value.data;
}
const current = async () => (await request()).contents[0];
const baseline = (content) => ({
  expected_current_version_id: content.current_version.version_id,
  expected_content_revision: content.content_revision,
});
async function finished(run, contentID) {
  for (let i = 0; i < 120; i++) {
    const state = await request(`/${contentID}/generations/${run.generation_id}`);
    if (!["pending", "running"].includes(state.status)) return state;
    await setTimeout(250);
  }
  assert.fail("worker did not terminalize within 30 seconds");
}
function sql(statement) {
  return execFileSync("docker", ["exec", "octo-summary-versioning-test-mysql", "mysql", "-uroot", "-N", "-B", "summary_versioning_execution", "-e", statement], { encoding: "utf8" }).trim();
}
let content = await current();
const id = content.content_id, prefix = `/${id}`;
assert.equal(content.capabilities.can_configure_schedule, true);
await request("", undefined, 403, { token: "fixture-outsider" });
const config = await request(`${prefix}/configuration`);
const spec = { ...config.spec, requirement: "Summarize the Alpha release", sources: [{ source_id: "group1", source_type: 1, confirmation: "user_confirmed" }] };
const saved = await request(`${prefix}/configuration`, {
  expected_config_revision: config.revision, spec,
  schedule: { enabled: true, interval_days: 7, interval_months: 0, run_time: "09:00", day_of_week: 0, day_of_month: 0 },
});
assert.equal(saved.generation, null, "saving a schedule must not generate immediately");
assert.equal(sql("SELECT COUNT(*) FROM summary_generation_run WHERE task_id=110"), "0");
assert.equal((await current()).current_version.content, content.current_version.content);
const generate = { ...baseline(content), expected_config_revision: saved.configuration.revision, idempotency_key: randomUUID() };
const accepted = await request(`${prefix}/generations/regenerate`, generate, 202);
assert.equal((await request(`${prefix}/generations/regenerate`, generate, 202)).generation_id, accepted.generation_id);
assert.equal((await finished(accepted, id)).applied, true);
content = await current();
assert.equal(content.current_version.version, 2);
assert.equal(content.current_version.citations.length, 1);
assert.match(content.current_version.content, /Alpha/);
const v2 = content.current_version.version_id;

// In-place edit preserves version identity and count.
const edited = await request(`${prefix}/edit`, { ...baseline(content), content: "Manual edit [1]" });
assert.equal(edited.version_id, v2);
assert.equal(sql("SELECT COUNT(*) FROM summary_personal_result_version WHERE task_id=110"), "2");
await request(`${prefix}/edit`, { ...baseline(content), content: "stale" }, 409);
content = await current();
const refined = await request(`${prefix}/generations/refine`, { ...baseline(content), feedback: "Shorter", idempotency_key: randomUUID() }, 202);
assert.equal((await finished(refined, id)).applied, true);
content = await current();
assert.equal(content.current_version.version, 3);

// Failed execution retains both saved configuration and current content.
const failConfig = await request(`${prefix}/configuration`, {
  expected_config_revision: saved.configuration.revision, spec: { ...spec, requirement: "fixture:fail" },
  generate: { ...baseline(content), expected_config_revision: saved.configuration.revision, idempotency_key: randomUUID() },
}, 202);
assert.equal((await finished(failConfig.generation, id)).status, "failed");
assert.equal((await current()).current_version.version_id, content.current_version.version_id);
assert.equal((await request(`${prefix}/configuration`)).state, "complete");
await request(`${prefix}/configuration`, { expected_config_revision: failConfig.configuration.revision, spec });

// Force only the synthetic fixture's next slot; the real worker polls it.
sql("UPDATE summary_schedule s JOIN summary_task t ON t.schedule_id=s.id SET s.next_run_at=DATE_SUB(NOW(), INTERVAL 2 DAY) WHERE t.id=110");
let scheduled;
for (let i = 0; i < 120; i++) {
  const next = await current();
  if (next.latest_generation?.operation_type === "scheduled_generate") { scheduled = next.latest_generation; break; }
  await setTimeout(250);
}
assert.ok(scheduled, "real worker did not claim due slot");
assert.equal((await finished(scheduled, id)).applied, true);
assert.equal(sql("SELECT COUNT(*) FROM summary_generation_run WHERE task_id=110 AND operation_type='scheduled_generate'"), "1");
content = await current();
assert.equal(content.current_version.version, 4);
assert.equal(sql("SELECT COUNT(*) FROM summary_task WHERE id=110"), "1");

const versions = await request(`${prefix}/versions`);
const source = versions.items.find((version) => version.version === 2);
const restored = await request(`${prefix}/restore`, { ...baseline(content), source_version_id: source.version_id });
assert.equal(restored.version_id, content.current_version.version_id);
assert.equal(restored.version, 4);
assert.equal(restored.content, "Manual edit [1]");
assert.equal(sql("SELECT COUNT(*) FROM summary_personal_result_version WHERE task_id=110"), "4");
console.log("PASS: actual API + Worker images; confirmed configuration, no implicit run, unique manual run, cited V2, edit CAS, refine V3, failed generation retention, actual scheduled V4, in-place restore");
