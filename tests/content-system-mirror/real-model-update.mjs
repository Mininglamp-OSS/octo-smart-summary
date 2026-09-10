// User-approved native-only repair. Never reseed data or print credentials.
// Run --verify-only first. Existing containers remain available for rollback.
import { execFileSync } from "node:child_process";
import { pathToFileURL } from "node:url";
import { setTimeout as delay } from "node:timers/promises";

export const modelKeys = [
  "LLM_API_URL", "LLM_API_KEY", "LLM_MODEL", "LLM_FALLBACK_MODELS",
  "LLM_TIMEOUT", "LLM_MAX_TOKENS", "LLM_TEMPERATURE", "LLM_ENABLE_THINKING",
  "KIMI_API_KEY",
];
export function inheritModel(target, source) {
  if (!source.LLM_API_URL || !source.LLM_API_KEY || !source.LLM_MODEL ||
      /fixture/i.test(source.LLM_API_URL) || source.LLM_MODEL === "fixture") {
    throw new Error("Source must have a configured real model.");
  }
  const env = { ...target };
  for (const key of modelKeys) {
    delete env[key];
    if (Object.hasOwn(source, key)) env[key] = source[key];
  }
  return env;
}

const suffix = "native-real-model-20260909";
const database = "summary_versioning_native_20260908";
const mysql = "octo-summary-versioning-test-mysql";
const docker = (args, options = {}) => {
  try {
    return execFileSync("docker", args, {
      encoding: "utf8", maxBuffer: 4 * 1024 * 1024,
      stdio: ["pipe", "pipe", "pipe"], ...options,
    });
  } catch {
    // execFile errors may contain sensitive environment/CLI output.
    throw new Error(`Docker ${args[0]} failed; inspect locally with redaction.`);
  }
};
const inspect = (name) => JSON.parse(docker(["inspect", name]))[0];
const environment = (container) => Object.fromEntries(container.Config.Env.map((entry) => {
  const i = entry.indexOf("=");
  return [entry.slice(0, i), entry.slice(i + 1)];
}));
const names = () => new Set(docker(["ps", "-a", "--format", "{{.Names}}"]).trim().split("\n"));
// Native Go connections store local Asia/Shanghai DATETIME values.
const query = (sql) => docker(["exec", mysql, "mysql", "-uroot", database, "-N", "-e",
  `SET time_zone='+08:00'; ${sql}`]).trim();

function assertIdle(apiStartedAt) {
  const checks = [
    ["formal generations", "SELECT COUNT(*) FROM summary_generation_run WHERE active_slot IS NOT NULL"],
    ["legacy tasks", "SELECT COUNT(*) FROM summary_task WHERE status IN (0,1,2)"],
    ["personal workers", "SELECT COUNT(*) FROM summary_personal_result WHERE worker_status IN (0,1)"],
    ["workspace turns", "SELECT COUNT(*) FROM agent_summary_turn WHERE status NOT IN ('completed','failed','cancelled')"],
    ["workspace sessions", "SELECT COUNT(*) FROM agent_summary_session WHERE active_turn_id<>0"],
    ["active schedules", "SELECT COUNT(*) FROM summary_schedule WHERE is_active=1 AND deleted_at IS NULL"],
    // Failed workspace calls can leave a legacy run labelled "running".
    // Require its authoritative terminal turn; never rewrite old run records.
    ["live legacy runs", `SELECT COUNT(*) FROM agent_summary_run r
      WHERE r.status IN ('created','running') AND r.updated_at >= FROM_UNIXTIME(${apiStartedAt})
      AND NOT EXISTS (SELECT 1 FROM agent_summary_session s
        JOIN agent_summary_turn t ON t.space_id=s.space_id AND t.user_id=s.user_id AND t.session_id=s.session_id
        WHERE t.user_id=r.user_id AND t.request_id=r.request_id
        AND r.session_id IN (
          s.agent_session_id,
          CONCAT('summaryws:',SHA2(CONCAT(TRIM(t.space_id),CHAR(0),TRIM(t.session_id),CHAR(0),t.scope_version),256)),
          CONCAT('summaryws:',SHA2(CONCAT(TRIM(t.space_id),CHAR(0),TRIM(t.session_id),CHAR(0),t.scope_version,CHAR(0),'replace',CHAR(0),TRIM(t.request_id)),256))
        )
        AND t.status IN ('completed','failed','cancelled'))`],
  ];
  for (const [label, sql] of checks) {
    if (query(sql) !== "0") throw new Error(`Native preview has ${label}; no update permitted.`);
  }
}

function contentFingerprint() {
  return query(`
    SELECT COUNT(*),COALESCE(SUM(id),0),COALESCE(SUM(COALESCE(current_result_id,0)),0),COALESCE(SUM(content_revision),0) FROM summary_task;
    SELECT COUNT(*),COALESCE(SUM(id),0),COALESCE(BIT_XOR(CRC32(content)),0) FROM summary_result;
    SELECT COUNT(*),COALESCE(SUM(id),0),COALESCE(BIT_XOR(CRC32(content)),0) FROM summary_personal_result;
    SELECT COUNT(*),COALESCE(SUM(id),0) FROM summary_personal_result_version;
  `);
}

async function healthy(url) {
  for (let attempt = 0; attempt < 30; attempt++) {
    try {
      const response = await fetch(url, { signal: AbortSignal.timeout(1500) });
      await response.body?.cancel();
      if (response.status === 200) return;
    } catch { /* bounded startup retries; never log response bodies */ }
    await delay(500);
  }
  throw new Error("Native health check failed.");
}

export async function main() {
  const targets = [
    { name: "octo-summary-native-api", source: "octo-unified-summary-local-api", port: "28361" },
    { name: "octo-summary-native-worker", source: "octo-unified-summary-local-worker" },
    { name: "octo-summary-native-web", image: `octo-web:${suffix}`, port: "28360" },
  ];
  const protectedNames = [
    "octo-unified-summary-local-api", "octo-unified-summary-local-worker",
    "octo-unified-summary-local-web", "octo-summary-versioning-execution-web",
    "summary-versioning-execution-api-1", "summary-versioning-execution-worker-1",
    "summary-versioning-execution-fixture-1",
  ];
  const protectedState = protectedNames.map((name) => {
    const item = inspect(name);
    return { name, id: item.Id, started: item.State.StartedAt };
  });
  const allNames = names();
  for (const target of targets) {
    target.old = inspect(target.name);
    target.backup = `${target.name}-before-${suffix}`;
    target.failed = `${target.name}-failed-${suffix}`;
    if (allNames.has(target.backup) || allNames.has(target.failed)) {
      throw new Error("Recovery containers exist; inspect before any repeat.");
    }
    if (!target.old.State.Running || target.old.Mounts.length) {
      throw new Error("Expected running mount-free native containers.");
    }
    const ports = Object.values(target.old.HostConfig.PortBindings ?? {}).flat();
    if (target.port ? ports.length !== 1 || ports[0].HostIp !== "127.0.0.1" ||
        ports[0].HostPort !== target.port : ports.length !== 0) {
      throw new Error("Unexpected native port binding.");
    }
    target.networks = Object.keys(target.old.NetworkSettings.Networks);
    if (!target.networks.includes("octo-alex_octo-net") ||
        target.networks.some((n) => !["octo-alex_octo-net", "octo-summary-versioning-test"].includes(n))) {
      throw new Error("Unexpected native networks.");
    }
    target.env = environment(target.old);
    if (target.source) {
      if (!target.env.MYSQL_DSN?.includes(`/${database}?`) ||
          target.env.SUMMARY_NOTIFY_ENABLED !== "false" ||
          target.env.LLM_API_URL !== "http://summary-versioning-execution-fixture-1:8089/v1" ||
          target.env.SUMMARY_CONTENT_WRITE_SPACES ||
          !target.networks.includes("octo-summary-versioning-test")) {
        throw new Error("Refusing a non-isolated or already-updated target.");
      }
      const source = inspect(target.source);
      if (!source.State.Running) throw new Error("Source is not running.");
      target.env = inheritModel(target.env, environment(source));
      target.image = target.old.Config.Image; // Business code is unchanged.
    }
    docker(["image", "inspect", target.image]);
  }
  const apiStartedAt = Math.floor(Date.parse(targets[0].old.State.StartedAt) / 1000);
  if (!Number.isSafeInteger(apiStartedAt)) throw new Error("Unexpected API start time.");
  assertIdle(apiStartedAt);
  const fingerprint = contentFingerprint();
  console.log(`Preflight passed: ${query("SELECT COUNT(*) FROM summary_task")} tasks retained; model values redacted.`);
  if (process.argv.includes("--verify-only")) return;

  const stopped = [];
  const renamed = [];
  try {
    // Stop intake first, then verify no request raced the preflight.
    for (const target of [targets[2], targets[0]]) {
      docker(["stop", "--time", "10", target.name]);
      stopped.push(target);
    }
    assertIdle(apiStartedAt);
    docker(["stop", "--time", "10", targets[1].name]);
    stopped.push(targets[1]);
    for (const target of targets) {
      docker(["rename", target.name, target.backup]);
      renamed.push(target);
    }
    for (const target of targets) {
      const args = ["create", "--name", target.name, "--network", target.networks[0],
        ...Object.keys(target.env).flatMap((key) => ["--env", key])];
      for (const [port, bindings] of Object.entries(target.old.HostConfig.PortBindings ?? {})) {
        for (const binding of bindings) args.push("-p", `${binding.HostIp}:${binding.HostPort}:${port}`);
      }
      if (target.name.endsWith("-worker")) args.push("--entrypoint", "/bin/summary-worker");
      args.push(target.image);
      docker(args, { env: { ...process.env, ...target.env } });
      for (const network of target.networks.slice(1)) docker(["network", "connect", network, target.name]);
      docker(["start", target.name]);
    }
    await healthy("http://127.0.0.1:28361/health");
    await healthy("http://127.0.0.1:28360/");
    for (const target of targets) {
      const current = inspect(target.name);
      const currentEnv = environment(current);
      if (!current.State.Running || Object.keys(currentEnv).length !== Object.keys(target.env).length ||
          Object.entries(target.env).some(([key, value]) => currentEnv[key] !== value)) {
        throw new Error("Replacement configuration verification failed.");
      }
    }
    if (contentFingerprint() !== fingerprint) throw new Error("Native content changed during update.");
    for (const protectedItem of protectedState) {
      const current = inspect(protectedItem.name);
      if (current.Id !== protectedItem.id || current.State.StartedAt !== protectedItem.started) {
        throw new Error("Protected stack changed during update.");
      }
    }
  } catch {
    for (const target of [...renamed].reverse()) {
      if (names().has(target.name)) {
        docker(["stop", "--time", "10", target.name]);
        docker(["rename", target.name, target.failed]);
      }
      docker(["rename", target.backup, target.name]);
    }
    for (const target of [...stopped].reverse()) docker(["start", target.name]);
    throw new Error("Native update failed; previous containers restored, no data reset.");
  }
  console.log("Native API/Worker now match source model settings; existing content and protected stacks unchanged.");
  console.log(`Previous containers retained with -before-${suffix}. Real Agent acceptance is still required.`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((error) => { console.error(error.message); process.exitCode = 1; });
}
