# Unified summary versioning implementation

Source: `agent-workflow-unified-summary-versioning-plan.md`, reviewed September 8, 2026. Backend base `391134c`; frontend base `9e33837a`.

## Behavior list

1. Keep one existing Workbench/detail entry. Main result and the caller's personal report have separate stable identities, versions and permissions.
2. Editing and restoring overwrite the current row with revision CAS, preserving its ID/number. Refining appends one version using frozen content and both citation pools, without retrieving chats.
3. Generation is persisted and idempotent; old content remains readable through failure/cancellation/restart. Conflicting output can be previewed and explicitly applied.
4. Regeneration and schedules require executable confirmed configuration. Changing configuration during a run affects only subsequent runs.
5. Historical/member citation privacy, existing summary URLs, Space isolation and legacy rows are retained. Destructive actions require explicit UI confirmation.

## File map

1. `internal/model/`, `migrations/sql/`: additive schema for revisions, snapshots, configuration, generation coordination and audit.
2. `internal/service/` and the existing repository package: namespaced targets, read DTOs, adapters, CAS writes, persisted runs and time/configuration validation.
3. `internal/api/handler/`, `internal/worker/`: authenticated shared entry points and legacy adapters; preserve personal identity, disable pruning only for enabled Spaces, synchronize scheduling/notifications.
4. Frontend `packages/dmworksummary/src/{Service,bridge,features,ui}/`: extend existing Workbench boundaries, do not create a second workbench or detail route.
5. Backend tests, frontend Service/bridge/UI tests and stories, implementation/verification notes: reproducible acceptance evidence.

## PR scope

One coordinated backend PR and one frontend PR for the summary-versioning feature, created only after local and mirror verification. No changes to octo-server, production deployment, physical version-table merging, arbitrary timezones, or historical member-report snapshot expansion. Additive compatibility first; do not activate new write semantics while legacy writers remain uncoordinated.

## Verification plan

1. Run Go tests (including CGO paths), frontend focused Vitest, TypeScript/build, i18n checks and `git diff --check`; distinguish pre-existing failures.
2. Use disposable MySQL 8 data to exercise migration replay, duplicate preflight, transaction CAS, concurrent requests and restart/idempotency behavior; do not treat SQLite as concurrency proof.
3. Build API/worker/web images from the isolated worktrees. Preserve the existing mirror configuration/images and data before switching test services; never modify production.
4. Test old personal content: edit, refine, restore, sixth version, configuration and schedule, reload/back/Space switch; verify failure/cancel/conflict and original content preservation.
5. Test team creator/member authorization and both citation namespaces; inspect affected UI in light/dark and zh-CN/en-US.

## Delivery gates

Compatibility reads → all writers/cleaners coordinated → enable content writes → configuration/scheduling → frontend activation → Docker acceptance → PRs.

No gate is considered passed until its evidence is recorded. The source plan estimates 13–19 person-days; initial repository preparation is not completion of that scope.
