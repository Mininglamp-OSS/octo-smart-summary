// Local full-host preview. Credentials stay in process environments; never
// print inspect payloads, DSNs or SQL dumps. Refuse to overwrite any target.
import { execFileSync } from "node:child_process";

const database = "summary_versioning_native_20260908";
const mysql = "octo-summary-versioning-test-mysql";
const sourceApi = "octo-unified-summary-local-api";
const sourceDb = "octo-unified-summary-local-db";
const api = "octo-summary-native-api";
const worker = "octo-summary-native-worker";
const web = "octo-summary-native-web";
const backendImage = "octo-summary-execution:configuration-20260908";
const frontendImage = "octo-web:versioning-native-20260908";
const docker = (args, options = {}) => execFileSync("docker", args, {
  encoding: "utf8", maxBuffer: 64 * 1024 * 1024, ...options,
});
const inspect = (name) => JSON.parse(docker(["inspect", name]))[0];
const inherited = Object.fromEntries(inspect(sourceApi).Config.Env.map((value) => {
  const index = value.indexOf("=");
  return [value.slice(0, index), value.slice(index + 1)];
}));
const dsn = inherited.MYSQL_DSN.match(/^([^:]+):(.+)@tcp\(([^)]+)\)\/([^?]+)/);
if (!dsn) throw new Error("Unrecognized source DSN; no changes made");
const existing = docker(["ps", "-a", "--format", "{{.Names}}"]).trim().split("\n");
if ([api, worker, web].some((name) => existing.includes(name))) {
  throw new Error("Native preview container already exists; inspect it, do not reseed");
}
docker(["image", "inspect", backendImage]);
docker(["image", "inspect", frontendImage]);
const query = (sql) => docker(["exec", mysql, "mysql", "-uroot", "-N", "-e", sql]);
if (query(`SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='${database}'`).trim()) {
  throw new Error("Native preview database already exists; inspect it, do not reseed");
}
const sourceEnv = { ...process.env, MYSQL_PWD: dsn[2] };
const sourceQuery = (sql) => docker(["exec", "-e", "MYSQL_PWD", sourceDb,
  "mysql", `-u${dsn[1]}`, dsn[4], "-N", "-e", sql], { env: sourceEnv });
const spaces = sourceQuery("SELECT DISTINCT space_id FROM summary_task").trim().split("\n").filter(Boolean);
if (spaces.length !== 1) throw new Error("Expected one local test Space; choose the rollout scope explicitly");
if (sourceQuery("SELECT COUNT(*) FROM summary_task WHERE status IN (0,1,2)").trim() !== "0") {
  throw new Error("Source has in-flight tasks; do not copy them into a running worker");
}
const dump = docker(["exec", "-e", "MYSQL_PWD", sourceDb, "mysqldump",
  `-u${dsn[1]}`, "--single-transaction", "--no-tablespaces", "--set-gtid-purged=OFF",
  "--skip-add-drop-table", dsn[4]], { env: sourceEnv });
query(`CREATE DATABASE ${database} CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci`);
docker(["exec", "-i", mysql, "mysql", "-uroot", database], { input: dump });
query(`UPDATE ${database}.summary_schedule SET is_active=0`);
console.log("Created a separate Summary data snapshot; copied schedules are paused.");

const environment = {
  ...inherited,
  MYSQL_DSN: `root@tcp(${mysql}:3306)/${database}?parseTime=true&loc=Asia%2FShanghai`,
  // Authentication and source permissions use the real LOCAL Octo server.
  // Only the model is synthetic; no production provider calls or notifications.
  OCTO_API_URL: "http://octo-server:8090",
  DMWORK_API_URL: "http://octo-server:8090",
  LLM_API_URL: "http://summary-versioning-execution-fixture-1:8089/v1",
  LLM_API_KEY: "local-synthetic-fixture",
  LLM_MODEL: "fixture",
  SUMMARY_CONTENT_EXECUTION_SPACES: spaces[0],
  SUMMARY_CONTENT_WRITE_SPACES: "",
  SUMMARY_NOTIFY_ENABLED: "false",
  MESSAGE_FETCH_BACKEND: "mysql",
  WORKER_POLL_INTERVAL_SECONDS: "2",
  WORKER_TRIGGER_URL: `http://${worker}:8082/internal/worker-trigger`,
  WORKER_API_CALLBACK_URL: `http://${api}:8081/internal/task-event`,
  TIMING_LOG_PATH: "/tmp/summary-timing.log",
  SUMMARY_REPORT_PATH: "/tmp/summary-report.log",
};
function create(name, image, env, extra = [], dualNetwork = true) {
  const args = ["create", "--name", name, "--network", "octo-alex_octo-net",
    ...Object.keys(env).flatMap((key) => ["--env", key]), ...extra, image];
  docker(args, { env: { ...process.env, ...env } });
  if (dualNetwork) docker(["network", "connect", "octo-summary-versioning-test", name]);
  docker(["start", name]);
  console.log(`Started ${name}`);
}
create(api, backendImage, environment, ["-p", "127.0.0.1:28361:8080"]);
create(worker, backendImage, environment, ["--entrypoint", "/bin/summary-worker"]);
create(web, frontendImage, {
  API_URL: "http://octo-server:8090",
  SUMMARY_API_URL: `http://${api}:8080`,
  NGINX_RESOLVER: "127.0.0.11",
}, ["-p", "127.0.0.1:28360:80"], false);
console.log("Full Octo Web: http://127.0.0.1:28360/ (existing 28140 and 28350 untouched)");
