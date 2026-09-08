# Unified summary versioning: compatibility slice verification

Date: September 8, 2026. Backend base `391134c`; frontend base `9e33837a`.

## Delivery status

The first compatibility slice is implemented and tested. The overall versioning
plan is **not complete**. No new write operation, schedule behavior, retention
policy or user-facing UI entry is enabled. No PR is ready.

## Implemented

1. Additive content/configuration/revision/snapshot schema and complete initial
   generation/audit table schema. Historical provenance stays `unknown`;
   current content revisions initialize from retained row counts.
2. Stable, namespaced content/version tokens and separate result/personal read
   adapters. Personal history uses `(task_id, user_id)`, not a transient
   participant/result row. Missing history is projected without GET writes.
3. Allowlisted content/version HTTP reads with permission filtering, integrity
   states, default 20-item pagination, duplicate-history repair errors and
   distinct message/member citation visibility.
4. Existing Workbench Service gains a formal-content read Service and strict
   bridge decoders. Preview identities and formal CAS baselines remain separate.
5. Reproducible MySQL and isolated image HTTP fixtures. Existing user worktrees
   and the 28140 mirror are untouched.

## Verification results

| Check | Result |
|---|---|
| `CGO_ENABLED=0 go test ./...` | Passed |
| Full `go test -race -shuffle=on -count=1 -timeout 10m ./...` with CI's native tokenizer library | Passed |
| Final focused content/handler/router tests with race detector and real MySQL DSN | Passed |
| `CGO_ENABLED=0 go vet ./...` and `git diff --check` | Passed |
| Summary package Vitest | 74 files, 1,162 tests passed |
| Frontend `pnpm i18n:check` | Passed |
| Frontend `pnpm --filter @octo/web build` | Passed |
| Frontend package typecheck | 5,901 diagnostics; identical to pristine upstream after path/line normalization, no new diagnostics |
| MySQL 8 old-schema upgrade and migration replay | Passed; legacy V7 with two retained rows initializes content revision 2 |
| MySQL admission/uniqueness | 12 separate connections produce exactly one active-slot winner; duplicate request/schedule/output rejected |
| API image HTTP smoke | Passed; seven versions, provisional V1, team privacy, namespace/Space/auth checks |
| GET mutation check after image smoke | Legacy personal task 102 still has zero formal version rows |

MySQL tests verify **schema-level** uniqueness, not the as-yet-unimplemented
cross-engine run lifecycle, runtime lease recovery or write CAS.

## Local environments

Backend: `/home/mlamp/worktrees/summary-versioning-backend`.
Frontend: `/home/mlamp/worktrees/summary-versioning-frontend`.
Branch in both: `codex/unified-summary-versioning`.

The API contract fixture runs at `127.0.0.1:28341`; its disposable MySQL is at
`127.0.0.1:28306`. The compose fixture and smoke commands are documented in
`tests/content-contract-mirror/README.md`. Synthetic credentials are fixture-only.
Scratch databases and containers are retained for inspection.

Raw test/build logs and the recovery checkpoint are local-only under `.codex/`.
The native tokenizer library is installed in a temporary directory, not a
system-wide library path.

## Next implementation gates

1. Transactional edit/restore/lazy normalization, current-row CAS and audits;
   never silently append hidden edit versions.
2. Shared generation admission/idempotency/expiry recovery/conflicting-output
   application, with every old API/worker/scheduler writer and pruner adapted.
3. Authorized executable configuration, transactionally consistent Workflow
   projections, common time resolution, schedules and notification semantics.
4. Actual Workbench UI actions, operation confirmations, run refresh/cancel/
   conflict behavior and user-visible revision/history rendering.
5. Full 28140 mirror acceptance using real historical samples and two accounts,
   including six-version retention and edit/refine/restore/schedule workflows;
   only then push to personal forks and create PRs.
