# Single-person execution mirror

This fixture runs the **real API and Worker entry points**, real MySQL source
retrieval, the existing personal generation pipeline, and formal version commits.
Only authentication and the HTTP model response are synthetic. Notifications are
disabled. It does not contact external models, use real accounts or touch 28140.
The test image uses the CGO-disabled tokenizer fallback; native CGO/race tests
are run separately. It is not the production Worker packaging acceptance.

## Initial setup (once, on a new fixture database)

The existing disposable MySQL container must be named
`octo-summary-versioning-test-mysql`, with an empty fixture-only root password,
and be on Docker network `octo-summary-versioning-test`.

```sh
docker exec octo-summary-versioning-test-mysql mysql -uroot -e \
  'CREATE DATABASE summary_versioning_execution CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci'
docker build -f tests/content-execution-mirror/Dockerfile \
  -t octo-summary-execution:configuration-20260908 .
docker compose -f tests/content-execution-mirror/compose.yml up -d fixture api
curl --fail http://127.0.0.1:28351/health
docker exec -i octo-summary-versioning-test-mysql mysql -uroot \
  summary_versioning_execution < tests/content-execution-mirror/seed.sql
docker compose -f tests/content-execution-mirror/compose.yml --profile worker up -d worker
node tests/content-execution-mirror/smoke.mjs
node tests/content-execution-mirror/resilience.mjs
```

Wait for the API health check before seeding so migrations finish first.
Setup and the initial smoke require a fresh fixture; **do not rerun the seed
over retained data**. There is intentionally no automatic reset or cleanup.
The resilience test can run again against the initialized fixture. It restarts
only `summary-versioning-execution-worker-1` and expires only its own synthetic
run lease, never leases in another database.

## Verified September 8, 2026

1. Configuration/source projection and revision CAS; saving a schedule makes
   no model call or content change. Requirements never default to the title.
2. Actual manual generation and duplicate admission return one run/output;
   cited V2, in-place edit, frozen-content refinement, failed-generation
   retention and actual due-slot scheduling work under the same summary ID.
3. Historical restore keeps the current version ID/number and row count.
   Conflict candidates can be previewed/applied without another version.
4. Cancellation fences a late model response. Worker restart plus expired
   lease recovery creates exactly one output.
5. Final retained fixture contains V1–V7; API/Web reloads create no new runs.

The browser fixture in the paired frontend repository serves its actual
`FormalContentEntry → feature → bridge → Service` components through nginx.
Its synthetic host is not a replacement product route or the complete
signed-in Web application. Web is on `127.0.0.1:28350`, API on
`127.0.0.1:28351`. All ports are loopback-only.

Keep `SUMMARY_CONTENT_EXECUTION_SPACES` unset on normal environments. The pilot
supports completed, creator-owned single-person content only. Team/group
execution, initial Agent V1 coordination, generation-event notifications and
real-history/two-account 28140 acceptance remain required before PR/rollout.
