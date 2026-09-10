import test from "node:test";
import assert from "node:assert/strict";
import { inheritModel, modelKeys } from "./real-model-update.mjs";

test("copies only approved model keys, retaining native isolation", () => {
  const target = {
    LLM_API_URL: "http://fixture/v1", LLM_API_KEY: "synthetic", LLM_MODEL: "fixture",
    LLM_TIMEOUT: "99", MYSQL_DSN: "native-db", SUMMARY_NOTIFY_ENABLED: "false",
    SUMMARY_CONTENT_WRITE_SPACES: "", OCTO_API_URL: "native-auth", WORKER_TRIGGER_URL: "native-worker",
  };
  const source = {
    LLM_API_URL: "https://model.invalid/v1", LLM_API_KEY: "test-only", LLM_MODEL: "real",
    MYSQL_DSN: "source-db", SUMMARY_NOTIFY_ENABLED: "true", SUMMARY_CONTENT_WRITE_SPACES: "*",
    OCTO_API_URL: "source-auth", WORKER_TRIGGER_URL: "source-worker",
  };
  const result = inheritModel(target, source);
  for (const key of Object.keys(target).filter((key) => !modelKeys.includes(key))) {
    assert.equal(result[key], target[key]);
  }
  for (const key of modelKeys) assert.equal(result[key], source[key]);
  assert.equal(Object.hasOwn(result, "LLM_TIMEOUT"), false);
  assert.equal(target.LLM_MODEL, "fixture");
});

test("rejects synthetic and incomplete sources without leaking their values", () => {
  for (const source of [
    {}, { LLM_API_URL: "http://fixture/v1", LLM_API_KEY: "secret-test", LLM_MODEL: "model" },
    { LLM_API_URL: "https://model.invalid/v1", LLM_MODEL: "model" },
  ]) {
    assert.throws(() => inheritModel({}, source), { message: "Source must have a configured real model." });
  }
});
