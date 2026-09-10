// Update only the existing native preview. Never copy/reseed data, print
// credentials, remove containers, or touch the original 28140 stack.
import { execFileSync } from "node:child_process";

const webOnly = process.argv.includes("--web-only");
const suffix = webOnly ? "native-actions-layout-20260909" : "native-actions-20260909";
const database = "summary_versioning_native_20260908";
const mysql = "octo-summary-versioning-test-mysql";
const targets = [
  { name: "octo-summary-native-api", image: `octo-summary-execution:${suffix}`, port: "28361" },
  { name: "octo-summary-native-worker", image: `octo-summary-execution:${suffix}` },
  { name: "octo-summary-native-web", image: `octo-web:${suffix}`, port: "28360" },
].filter((target) => !webOnly || target.name === "octo-summary-native-web");
const docker = (args, options = {}) => execFileSync("docker", args, {
  encoding: "utf8", maxBuffer: 1024 * 1024, stdio: ["pipe", "pipe", "pipe"], ...options,
});
const inspect = (name) => JSON.parse(docker(["inspect", name]))[0];
const names = new Set(docker(["ps", "-a", "--format", "{{.Names}}"]).trim().split("\n"));
const query = (sql) => docker(["exec", mysql, "mysql", "-uroot", database, "-N", "-e", sql]).trim();

// Resolve every target before stopping anything. Credentials stay in memory.
for (const target of targets) {
  target.old = inspect(target.name);
  target.backup = `${target.name}-before-${suffix}`;
  if (names.has(target.backup) || names.has(`${target.name}-failed-${suffix}`)) {
    throw new Error("Update recovery containers already exist; inspect before retrying.");
  }
  docker(["image", "inspect", target.image]);
  if (!target.old.State.Running || target.old.Mounts.length > 0) {
    throw new Error("Expected a running, mount-free native preview.");
  }
  const env = Object.fromEntries(target.old.Config.Env.map((entry) => {
    const index = entry.indexOf("=");
    return [entry.slice(0, index), entry.slice(index + 1)];
  }));
  target.env = env;
  if (target.port) {
    const ports = Object.values(target.old.HostConfig.PortBindings ?? {}).flat();
    if (ports.length !== 1 || ports[0].HostIp !== "127.0.0.1" || ports[0].HostPort !== target.port) {
      throw new Error("Unexpected native preview port binding.");
    }
  }
  if (target.name !== "octo-summary-native-web" &&
      (!env.MYSQL_DSN?.includes(`/${database}?`) || env.SUMMARY_NOTIFY_ENABLED !== "false" ||
       env.LLM_API_URL !== "http://summary-versioning-execution-fixture-1:8089/v1")) {
    throw new Error("Refusing to update a non-isolated API/worker.");
  }
  target.networks = Object.keys(target.old.NetworkSettings.Networks);
  if (target.networks.some((name) => !["octo-alex_octo-net", "octo-summary-versioning-test"].includes(name))) {
    throw new Error("Unexpected native preview network.");
  }
}
if (query("SELECT COUNT(*) FROM summary_generation_run WHERE active_slot IS NOT NULL") !== "0") {
  throw new Error("Native preview has active generations; wait for completion before updating.");
}
console.log(`Preflight passed: existing native preview only; ${query("SELECT COUNT(*) FROM summary_task")} tasks retained.`);
if (process.argv.includes("--verify-only")) process.exit(0);

const replaced = [];
try {
  for (const target of [...targets].reverse()) {
    docker(["stop", "--time", "10", target.name]);
    docker(["rename", target.name, target.backup]);
    replaced.push(target);
  }
  for (const target of targets) {
    const args = ["create", "--name", target.name, "--network", target.networks[0],
      ...Object.keys(target.env).flatMap((key) => ["--env", key])];
    for (const [containerPort, bindings] of Object.entries(target.old.HostConfig.PortBindings ?? {})) {
      for (const binding of bindings) args.push("-p", `${binding.HostIp}:${binding.HostPort}:${containerPort}`);
    }
    if (target.name.endsWith("-worker")) args.push("--entrypoint", "/bin/summary-worker");
    args.push(target.image);
    docker(args, { env: { ...process.env, ...target.env } });
    for (const network of target.networks.slice(1)) docker(["network", "connect", network, target.name]);
    docker(["start", target.name]);
    console.log(`Updated ${target.name}; previous container retained as ${target.backup}.`);
  }
} catch {
  // No deletion, even on failure. Retain failed replacements for inspection.
  for (const target of [...replaced].reverse()) {
    const currentNames = new Set(docker(["ps", "-a", "--format", "{{.Names}}"]).trim().split("\n"));
    if (currentNames.has(target.name)) {
      docker(["stop", "--time", "10", target.name]);
      docker(["rename", target.name, `${target.name}-failed-${suffix}`]);
    }
    docker(["rename", target.backup, target.name]);
    docker(["start", target.name]);
  }
  throw new Error("Native update failed; restored previous containers. No data was reseeded.");
}
console.log("Native preview updated on 28360/28361; original 28140 and diagnostic 28350 unchanged.");
