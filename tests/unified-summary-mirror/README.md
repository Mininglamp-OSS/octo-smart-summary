# Unified summary continue-optimize mirror

This fixture runs the **real API and Worker entry points**, real MySQL source
retrieval, the real personal generation pipeline, formal version commits, and —
unlike the sibling mirrors — the **real notification state machine**. Only
authentication, the HTTP model response and octo-server's `/v1/internal/notify`
endpoint are synthetic. It does not contact external models, use real accounts,
or touch 28140.

It exists because `tests/content-execution-mirror` runs with
`SUMMARY_NOTIFY_ENABLED=false`, which makes the continue-optimize notification
gate a no-op there — neither its presence nor its absence can be accepted.

## Two mirrors: hermetic (`compose.yml`) and full-Octo (`compose.octo.yml`)

`compose.yml` is this hermetic fixture — synthetic auth, model and notify sink,
its own seeded IM tables — and it is what `smoke.mjs` and the frontend's
`browser.mjs` accept. It never touches real data or real accounts.

`compose.octo.yml` is the hands-on counterpart: the **same API/Worker image**
wired into the real dev stack (`octo-server` for auth, that stack's IM database
read-only through the `summary_reader` grant, the real LLM gateway) and fronted
by the **whole** octo-web production bundle, so the feature can be walked in the
real product UI with a real login:

```sh
docker compose -p summary-unified-octo \
  --env-file /home/mlamp/octo-deployment/docker/.env \
  -f tests/unified-summary-mirror/compose.octo.yml up -d
# web http://127.0.0.1:28370/   api http://127.0.0.1:28373/health
```

Secrets come from that env file at run time and are never written into the
compose file. Its summary database is our own throwaway one
(`summary_unified_octo` on `octo-summary-versioning-test-mysql`); nothing writes
to the dev stack's database except through octo-server's normal API. Notifications
are **off** there on purpose — real cards would reach real accounts — so the
continue-optimize notification gate stays accepted here, against the synthetic
sink, where the delivery count can be asserted.

Ports: `28370` full-Octo web, `28373` its API; `28371` this fixture's API,
`28372` the fixture web host.

## Initial setup (once, on a new fixture database)

The existing disposable MySQL container must be named
`octo-summary-versioning-test-mysql`, with an empty fixture-only root password,
and be on Docker network `octo-summary-versioning-test`.

```sh
docker exec octo-summary-versioning-test-mysql mysql -uroot -e \
  'CREATE DATABASE summary_versioning_unified CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci'
docker build -f tests/content-execution-mirror/Dockerfile \
  -t octo-summary-unified:continue-optimize-20260910 .
docker compose -f tests/unified-summary-mirror/compose.yml up -d notifyfixture api
curl --fail http://127.0.0.1:28371/health
# --default-character-set=utf8mb4 is not optional: `docker exec` gives the client
# LANG=C, so it otherwise negotiates latin1 and the seeded Chinese messages and
# agent drafts land double-encoded (the model stub then matches no keyword and
# every summary comes out empty).
docker exec -i octo-summary-versioning-test-mysql mysql -uroot \
  --default-character-set=utf8mb4 \
  summary_versioning_unified < tests/unified-summary-mirror/seed.sql
docker compose -f tests/unified-summary-mirror/compose.yml --profile worker up -d worker
node tests/unified-summary-mirror/smoke.mjs
```

Wait for the API health check before seeding so migrations finish first. The
smoke test consumes both seeded agent drafts (a successful save deletes its
session's messages) and asserts absolute delivery counts, so **do not rerun it
over retained data** — recreate the database and reseed. There is intentionally
no automatic reset or cleanup.

## What it accepts

1. A from-scratch agent save carries no `referenced_task_ids` and delivers no
   notification, and writes no `summary_notification` row.
2. A continue-optimize save (with `referenced_task_ids`) creates a **new**
   summary, inherits its origin channel from the referenced summary, leaves the
   referenced summary untouched, and delivers exactly one `completed` card to the
   creator — through the real claim/deliver/mark state machine, verified as
   `summary_notification.status='sent'` and as a request the synthetic
   octo-server actually received with a valid `X-Internal-Token`.
3. The new agent summary is born with an incomplete generation configuration:
   `can_configure_schedule` / `can_regenerate_with_config` open,
   `can_schedule` / `can_regenerate_direct` closed.
4. 补齐配置 (saving a valid spec) opens `can_schedule` and
   `can_regenerate_direct` without changing the content, and then manual
   regeneration and a real due-slot scheduled run each produce a new version of
   the **same** summary (V2, V3) rather than another summary.

Keep `SUMMARY_CONTENT_EXECUTION_SPACES` unset on normal environments, and note
that `SUMMARY_NOTIFY_TOKEN` here is a fixture-only constant shared with
`provider.mjs`.

## The agent conversation

`provider.mjs` also speaks the `summary_workspace` profile's tool protocol: when a
request carries the `emit_summary_response` tool it answers with that tool call
(mirroring the `result_type` the handler pinned in the prompt) instead of prose.
The profile rejects a free-text final answer, so without this every agent turn
fails with a 500 — and 继续优化 (reference the current summary → agent
conversation → save a **new** summary) is exactly the flow that runs through it.
With it, the whole path is walkable by hand in the web fixture, and the save lands
a new summary carrying `referenced_task_ids` plus one delivered notification, with
the referenced summary untouched.

## The UI half

The web repo's `tests/unified-summary-mirror/` serves the real summary frontend
over this stack (`127.0.0.1:28370`, proxying `/summary/` to `api:8080` on the
shared network). It boots the production runtime and offers both hosts — the
unified workspace (`?mount=workspace`) and the IM chat side panel (`?mount=chat`,
opened through the real channel-header button) — and its `browser.mjs` walks the
same two summaries in a browser through both: that continue-optimize is its own
entry which leaves the detail, and that 重新生成 / 配置与定时更新 are the
same-summary entries — with 重新生成 shut until the agent summary's config is
complete. Run this smoke first; that walkthrough reads the summaries it creates.
