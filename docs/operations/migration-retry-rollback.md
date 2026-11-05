# Summary rollout: migration retry and application rollback

This change hardens the DDL in `20261019-01-optimization-save.sql`,
`20261020-01-summary-citation-owner.sql`, and
`20261021-01-summary-search-normalized.sql`. The intervening personal provenance
backfill already has a repeatable, matching-payload-only update and is tested
without changing its SQL. Migration IDs and the resulting schema stay unchanged.

## Failure and retry

MySQL DDL implicitly commits: a migration file is not an atomic transaction.
After a process/connection failure, columns or an index can exist without its
`gorp_migrations` receipt. Each DDL now checks its expected definition before
executing. A compatible existing object is skipped; an incompatible same-name
object fails rather than recording an incorrect schema as complete. These guards
cover the four pending rollout migrations, not every historical migration.

After resolving the failure (for example connectivity, permissions, or capacity),
restart the same new image with migration enabled. Keep the existing migration
receipts and schema; do not mark a failed file complete manually. The existing
migration lock serializes startup migrations, but does not stop business writes.
A failing instance exits before serving. Repeated failures still require diagnosis;
retry does not repair an incompatible schema or inadequate permissions.

Search normalization uses the existing 500-row transactions. Completed batches
survive interruption, and the next startup resumes NULL fields without changing
`updated_at`. A failure here can occur after all SQL migration receipts exist;
startup still reruns the backfill. Do not use the reported migration count as a
complete description of partial progress.

## Application rollback (retaining schema)

1. Before rollout, take and verify a recoverable database backup, record the API
   and Worker image digests and deployment settings, and rehearse on test first.
2. If rollback is needed, stop/drain new API and Worker writers and retain the
   expanded schema and migration records. Do not run these migrations' `Down`
   sections: they remove columns and their data; they are not a lossless rollback.
3. Start the verified old API **and** Worker images with `SKIP_MIGRATION=true`.
   Otherwise the old migration source rejects the unknown new migration IDs and
   startup exits. Apply this only as an explicit rollback setting, not the default
   for a subsequent new release. Check startup, reads and representative writes.
4. When returning to the new release, remove the rollback skip setting. Startup
   completes any pending migrations and reruns the search backfill. Confirm ready
   instances and migration receipts before restoring traffic.

Rolling back application binaries does not undo backfilled data. Old writes can
create results without new snapshot/owner metadata; do not promise new workbench
behavior or complete provenance while old code is running. If actual data recovery
is needed, use a separately planned backup/PITR recovery with writes paused;
application rollback alone is not data restoration.

## Isolated verification (2026-11-05)

No test/prod database was modified. Tests ran on disposable MySQL Community
8.0.46/InnoDB, not the deployed CynosDB 8.0.30 build. Production-scale lock duration,
DDL privileges, storage pressure and engine-specific behavior still need test
rollout validation.

- SQL error injection and server-side connection termination at every statement
  boundary of all four migrations, including after the last SQL but before its
  receipt; restart with a fresh connection, complete, then rerun as a no-op.
- Existing content retained, matching personal provenance copied and mismatched
  history left unknown; trusted ownership backfill retried without overwriting
  an existing owner; incompatible column/index definitions rejected.
- Search backfill failed in its second batch: the first 500 rows stayed committed,
  then retry completed all rows and retained timestamps.
- Exact old API/Worker images below: old API bootstrapped the 44-migration schema;
  current migration code applied four migrations and then reported zero on repeat.
  Old API startup with migrations enabled failed on unknown migration IDs as
  expected. With skip enabled, API/Worker health checks passed; authenticated
  history reads, editing, refinement and version restoration passed through the
  actual old API. Existing new metadata remained; old refinement created a result
  with NULL snapshot and empty owner, as expected. Current migration code then
  ran again successfully with zero migrations.

Old images tested:

```text
smart-summary-api:v1.15.1
sha256:b2d2542b6191e429b13c5c2ecf7b75f05241a343980e73f0ba4f197f2bc42209
smart-summary-worker:v1.15.1
sha256:0f03a5998d0412997a5473aec1f4cedcf2755334fd7cfa6ade8cf7a78a7ce488
```

Both images came from `tbj7-xtiao-tcr1.tencentcloudcr.com/dmwork`.
The rollback fixture used a private Docker network, synthetic data, mock auth and
mock LLM (`DeepSeek-V4-Pro`, no fallback). Worker validation covered startup/health,
not a full message-fetch/generation pipeline. Returning to the new release was
validated through its real migration runner, not a complete redeployment rehearsal.

## Reproduce migration tests

Use a disposable loopback MySQL database named `summary_migration_*` and provide
`SUMMARY_MIGRATION_MYSQL_TEST_DSN` through the test environment (do not commit
credentials). The tests **drop and recreate their fixture tables** and must not
point to a database with valuable data. Loopback/name checks are guardrails, not
proof that a database is disposable. Keep other test processes off that database.

```sh
go test ./internal/db -count=1 -timeout=10m \
  -run 'Test(RolloutMigrations|CitationOwnerBackfillRetry|SearchBackfillRetry)'
```

Without the variable these tests skip. The normal SQLite suite cannot establish
MySQL DDL interruption safety. The isolated image checks above are a manual
integration record, not part of this opt-in Go test command.
