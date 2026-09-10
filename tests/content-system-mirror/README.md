# Native Octo mirror

This is the normal Octo Web shell, API and Worker, not the component fixture.
Only containers named `octo-summary-native-{web,api,worker}` belong to it.
Web: 127.0.0.1:28360; API: 127.0.0.1:28361.
Database: `summary_versioning_native_20260908`.

## September 9, 2026 real-model repair

The user authorized reusing the existing 28140 model configuration for 28360.
`real-model-update.mjs` has **already completed**. Do not rerun it or the
older `start.mjs`/`update.mjs`; recovery-container checks intentionally reject
repetition. Do not overwrite existing data to repeat acceptance.

The script copies only allowlisted model environment keys in memory, using
the corresponding 28140 API/Worker as the sources. Docker receives environment
variable names, not secret values in arguments or generated files. Native DB,
auth/source services, disabled notifications and rollout settings are retained.
Old backend business images are reused; only Web is rebuilt.

Preflight checks exact containers/ports/networks, dormant schedules, formal/
legacy generations, personal workers and durable workspace turns/sessions.
Legacy runs from before the current process are not live; newer abandoned
workspace runs require an exact trusted session identity and terminal turn
match. The per-connection SQL timezone matches the Go Asia/Shanghai DATETIME
storage. No legacy state is rewritten.

It stops intake, rechecks idle state, retains backups, verifies health/env/
content fingerprints and protected stacks, and restores old containers on
failure. Previous containers are retained as:

- `octo-summary-native-web-before-native-real-model-20260909`
- `octo-summary-native-api-before-native-real-model-20260909`
- `octo-summary-native-worker-before-native-real-model-20260909`

Rollback would also restore the synthetic model. Before any manual rollback,
check active requests and obtain agreement on this loss of real-model behavior;
do not delete current containers or revert the database. Existing tests/data
belong to the user. Never print full Docker inspect, credentials or dumps.

## Reproducible checks

```bash
node --test tests/content-system-mirror/real-model-update.test.mjs
node --check tests/content-system-mirror/real-model-update.mjs
```

Only before an approved fresh deployment, with a newly reviewed unique suffix,
run `--verify-only`, then the update once. The completed suffix is not reusable.
Full-host browser acceptance must follow; a successful configuration match
alone does not prove normal Agent/summary usability.

Acceptance created only task 60 (`镜像验收-真实模型-运维总结-20260909`):
real Agent fetch/map/reduce→preview→save V1, then real Worker refinement→V2,
both with citations and non-Alpha content. V1 is retained. Old tasks/versions
were not regenerated; schedules and notifications remained disabled. The
pre-existing explanation keyword-routing limitation is documented in the
frontend implementation note.
