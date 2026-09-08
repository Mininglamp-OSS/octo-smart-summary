# Unified summary versioning verification

Date: September 8, 2026. Backend base `391134c`; frontend base `2a41ee1d`.
Local code commits: backend `c928ab6`, frontend `10d7ae85`. The frontend was
rebased from the initial `9e33837a` baseline after upstream PR #1640 landed
during this implementation; the checks below were repeated on the new base.

## Delivery status

The compatibility, transactional service, legacy-write fences and durable refine
Worker slices are implemented and tested. The overall versioning plan is **not
complete**. Production command routes remain unmounted and all user-facing write
capabilities remain false. Old managed operations now fail safely; complete
generation, collaboration and scheduling still need actual coordinated adapters.
Keep `SUMMARY_CONTENT_WRITE_SPACES` unset outside isolated fixtures until those
adapters and the UI are ready. No PR is ready.

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
| Summary package Vitest | 82 files, 1,230 tests passed |
| Frontend `pnpm i18n:check` | Passed |
| Frontend `pnpm --filter @octo/web build` | Passed |
| Frontend package typecheck | 6,025 diagnostics; identical to pristine `2a41ee1d` upstream after path/line normalization, no new diagnostics |
| MySQL 8 old-schema upgrade and migration replay | Passed; legacy V7 with two retained rows initializes content revision 2 |
| MySQL admission/uniqueness | 12 separate connections produce exactly one active-slot winner; duplicate request/schedule/output rejected |
| API image HTTP smoke | Passed; seven versions, provisional V1, team privacy, namespace/Space/auth checks |
| GET mutation check after image smoke | Legacy personal task 102 still has zero formal version rows |

The initial compatibility checks above verified schema-level uniqueness. The
continuation below adds runtime tests, without claiming all legacy writers now
use the coordinator.

## Transactional service continuation (September 8, 2026)

1. Result/personal edit and restore require current-version/revision CAS.
   Provisional V1 materializes only inside the first write; evidenced legacy
   divergence is normalized with audit, never a hidden edit version. Restore
   preserves the current ID/number and historical source, copies both citation
   pools and the source snapshot, and leaves configuration/schedules unchanged.
2. Durable refinement freezes authorized content/evidence and supports request
   idempotency, admission slots, expiry recovery, execution fencing, cancellation,
   single output, persisted conflict candidates and explicit application. The
   model adapter has no retrieval/tools interface and emits no IM notification.
   Permission revocation terminates and releases the run atomically.
3. Shared time resolvers cover absolute, rolling, natural calendar and incremental
   windows with explicit Shanghai, half-open boundaries. Late schedule slots
   preserve recurrence phase and month-end anchors. They are not yet wired to
   Workflow retrieval or the production scheduler.
4. Command handlers are mounted only in isolated tests. Read DTOs expose active
   runs/pending candidates and add an authenticated generation GET endpoint.
   Frontend Service/bridge contracts cover commands and run response validation;
   no UI actions are connected or visually accepted.
5. A dedicated Docker test image runs the service runtime suite against isolated
   MySQL, without touching 28140, external LLMs, credentials or notifications.

### Runtime verification

| Check | Result |
|---|---|
| Backend full `go test -race ./...` with native tokenizer library | Passed |
| MySQL 12-connection edit CAS | One successful edit, no duplicate lazy V1 |
| MySQL 12-connection runtime admission | One live content run |
| MySQL 12 concurrent completion callbacks | All acknowledge one output version |
| MySQL audit-failure injection | Body/revision and lazy baseline roll back together |
| Both storage adapters | Edit/refine/restore, conflict/apply, cancellation, expired-token fencing and seven retained versions passed |
| Task/content admission primitive | Distinct children coexist; independent runs and duplicate children rejected |
| Frozen model adapter | Uses frozen input; team creator never receives member message evidence |
| Time resolver tests | Rolling 60 days, calendar boundaries/leap day, incremental gaps and weekly/month-end/cron late slots passed |
| Final focused backend tests, with race detector and real MySQL | Service, handler and router passed |
| Final Summary Vitest suite | 82 files, 1,236 tests passed |
| Frontend production build and i18n | Passed |
| Frontend typecheck | Same 6,025 diagnostics as pristine upstream, no added/removed diagnostics after path/line/footer normalization |
| Existing isolated HTTP compatibility smoke | Passed again against the original compatibility API image |
| Dedicated runtime Docker image | MySQL/runtime/time suites passed with Shanghai process and DSN timezone |

Runtime image: `octo-summary-runtime-tests:versioning-20260908`.
Retained container: `octo-summary-runtime-tests-20260908` (exited successfully).
Commands are in `tests/content-contract-mirror/README.md`.

This is a service integration-test image, not a deployed Web workflow. It does
not prove real Workflow execution, configuration authorization/projection,
applied-success schedule watermarks, legacy prune/requeue safety, UI confirmation
flows or complete 28140 acceptance.

## Legacy-write fence and Worker continuation (September 8, 2026)

1. Added forward-only `20260908-03` migration and a sticky task protocol marker.
   Successful coordinated normalization enrolls atomically; an audit/write failure
   rolls back enrollment. The migration recognizes earlier generation/audit rows.
   Withdrawing runtime flags cannot re-enable old restore/prune behavior.
2. API and Worker share a separate, exact `SUMMARY_CONTENT_WRITE_SPACES` allowlist.
   Read-only enrollment still cannot enable writes or change retention. Both
   history stores retain all managed versions; cleaner decisions and deletes
   serialize with enrollment using the task lock.
3. Old edit/refine/restore/draft/regenerate commits, collaboration mutations,
   personal callbacks/failure updates, task claims and scheduler requeue/retry
   paths are fenced. Old refine rechecks after the model returns. Rejected old
   scheduling does not clear bodies, reset pointers/revisions or advance time
   anchors. Schedule-touching transactions keep schedule-before-task lock order.
4. `ContentGenerationWorker` is wired into the real worker entry point. It scans
   committed refinements at startup and periodically without an HTTP wakeup,
   uses bounded non-blocking pool admission, and leaves interrupted runs
   recoverable after lease expiry. Deleting enrolled tasks cancels active runs
   in the deletion transaction, fencing late results.
5. Added API, service, Worker and real-MySQL regression coverage. The SQLite
   concurrent-failure fixture now uses `BEGIN IMMEDIATE` because SQLite ignores
   `FOR UPDATE`; an additional 12-connection MySQL test proves production retry
   accumulation with the new task lock.

These fences are **not** implementations of coordinated full generation,
member requeue, configuration projection or scheduled execution. In particular,
the current rollout flag blocks old initial/full generation in its Space.
Do not enable it for a user Space as a completed feature.

| Final check for this slice | Result |
|---|---|
| Full backend `go test -race ./... -count=1`, with real MySQL DSN | Passed |
| `CGO_ENABLED=0 go vet ./...` | Passed |
| Focused API/Worker/service race tests | Passed, including post-LLM legacy rejection and soft-delete 404 regression |
| Concurrent MySQL enrollment / old writer / history cleaner | Old writer rejected; seven versions retained |
| Deletion/cancellation transaction | Failed deletion rolls back cancellation; successful deletion rejects late output |
| Real Worker startup, flag isolation, restart and saturation | Passed against MySQL; one committed output after recovery |
| Legacy concurrent failure accounting | 12 connections produce exactly 12 retry increments |
| Expanded runtime image, service binary | Passed MySQL runtime, enrollment/deletion and time tests |
| Expanded runtime image, Worker binary | Passed all four `TestContentWorker` cases against MySQL |

Expanded image: `octo-summary-runtime-tests:legacy-fence-20260908`.
Retained successful containers: `octo-summary-legacy-fence-tests-20260908`
and `octo-summary-content-worker-tests-20260908`.
This still runs test binaries, **not** Web/API end-to-end acceptance.
The frontend worktree remains unchanged at `6b3ed202`; 28140 and the older
28341 compatibility API remain unchanged. No external model call, production
write, notification, branch push or PR was performed.

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

1. Replace compatibility rejection with actual unified adapters for legacy
   commands, Agent formal V1, collaboration and full generation. Preserve
   personal IDs/current content during requeue before enabling commands.
2. Add coordinated full-generation commits while preserving task-level exclusion
   and parallel team-member execution; refine polling is now connected.
3. Authorized executable configuration, transactionally consistent Workflow
   projections, common time resolution, schedules and notification semantics.
4. Actual Workbench UI actions, operation confirmations, run refresh/cancel/
   conflict behavior and user-visible revision/history rendering.
5. Full 28140 mirror acceptance using real historical samples and two accounts,
   including six-version retention and edit/refine/restore/schedule workflows;
   only then push to personal forks and create PRs.
