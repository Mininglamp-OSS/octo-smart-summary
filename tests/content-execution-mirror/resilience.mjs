import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";
import { setTimeout } from "node:timers/promises";

const root = "http://127.0.0.1:28351/api/v1/summaries/110/contents";
const headers = { token: "fixture-owner", "X-Space-Id": "execution-fixture", "Content-Type": "application/json" };
async function request(path = "", data, status = 200) {
  const res = await fetch(root + path, { headers, ...(data ? { method: "POST", body: JSON.stringify(data) } : {}) });
  const body = await res.json();
  assert.equal(res.status, status, JSON.stringify(body));
  return body.data;
}
const current = async () => (await request()).contents[0];
const base = (content) => ({ expected_current_version_id: content.current_version.version_id, expected_content_revision: content.content_revision });
const content = await current(), prefix = `/${content.content_id}`;
async function waitRun(run, terminal = true) {
  for (let i = 0; i < 120; i++) {
    const value = await request(`${prefix}/generations/${run.generation_id}`);
    if (terminal ? !["pending", "running"].includes(value.status) : value.status === "running") return value;
    await setTimeout(250);
  }
  assert.fail("run failed to progress within 30 seconds");
}
async function queueSlow() {
  const config = await request(`${prefix}/configuration`);
  const now = await current();
  return (await request(`${prefix}/configuration`, {
    expected_config_revision: config.revision, spec: { ...config.spec, requirement: "fixture:slow" },
    generate: { ...base(now), expected_config_revision: config.revision, idempotency_key: randomUUID() },
  }, 202)).generation;
}
function sql(query) {
  return execFileSync("docker", ["exec", "octo-summary-versioning-test-mysql", "mysql", "-uroot", "-N", "-B", "summary_versioning_execution", "-e", query], { encoding: "utf8" }).trim();
}
const before = Number(sql("SELECT COUNT(*) FROM summary_personal_result_version WHERE task_id=110"));
// Concurrent content CAS should preserve an unapplied candidate, not overwrite.
const conflict = await queueSlow();
await waitRun(conflict, false);
await request(`${prefix}/edit`, { ...base(await current()), content: "Concurrent fixture edit [1]" });
assert.equal((await waitRun(conflict)).status, "conflict");
assert.equal((await current()).current_version.content, "Concurrent fixture edit [1]");
const page = await request(`${prefix}/versions`);
assert.ok(page.items.some((v) => v.pending_application && v.generation_id === conflict.generation_id));
await request(`${prefix}/generations/${conflict.generation_id}/apply`, base(await current()));
assert.equal(Number(sql("SELECT COUNT(*) FROM summary_personal_result_version WHERE task_id=110")), before + 1);

const cancelled = await queueSlow();
await waitRun(cancelled, false);
await request(`${prefix}/generations/${cancelled.generation_id}/cancel`, {});
assert.equal((await waitRun(cancelled)).status, "cancelled");
await setTimeout(3000); // let the synthetic late result arrive
assert.equal(Number(sql("SELECT COUNT(*) FROM summary_personal_result_version WHERE task_id=110")), before + 1);

// Restart the isolated worker, then expire only this synthetic run's lease.
const interrupted = await queueSlow();
const running = await waitRun(interrupted, false);
execFileSync("docker", ["restart", "--time", "1", "summary-versioning-execution-worker-1"], { stdio: "pipe" });
assert.match(running.generation_id, /^[0-9a-f-]{36}$/);
sql(`UPDATE summary_generation_run SET lease_until=DATE_SUB(NOW(), INTERVAL 1 MINUTE) WHERE id='${running.generation_id}' AND task_id=110 AND status='running'`);
const recovered = await waitRun(interrupted);
assert.equal(recovered.status, "completed");
assert.equal(recovered.applied, true);
assert.equal(Number(sql("SELECT COUNT(*) FROM summary_personal_result_version WHERE task_id=110")), before + 2);
assert.equal(Number(sql(`SELECT COUNT(*) FROM summary_personal_result_version WHERE generation_id='${running.generation_id}' AND task_id=110`)), 1);
const config = await request(`${prefix}/configuration`);
await request(`${prefix}/configuration`, { expected_config_revision: config.revision, spec: { ...config.spec, requirement: "Summarize the Alpha release" } });
console.log("PASS: image conflict candidate/application, cancellation with late model response, worker restart and expired lease recovery, single output per recovered run");
