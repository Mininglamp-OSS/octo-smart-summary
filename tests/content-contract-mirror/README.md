# Formal-content compatibility mirror

This is an isolated API/MySQL contract fixture, **not** the full 28140
personal/team workflow acceptance environment. No real credentials, LLM calls,
worker triggers, notification delivery or production data are used.

1. Start a disposable MySQL 8 container named
   `octo-summary-versioning-test-mysql`, with database `summary_versioning_test`
   and an empty root password, bound only to `127.0.0.1:28306`.
2. Create Docker network `octo-summary-versioning-test` and connect that
   disposable DB container. Build the repository's `Dockerfile.api` with tag
   `octo-summary-api:versioning-contract-20260908`.
3. Start this compose file using project name `summary-versioning-contract`.
4. Apply `seed.sql` to **only** `summary_versioning_test`; run
   `node tests/content-contract-mirror/smoke.mjs`.
5. Check that task 102 still has no formal personal versions after the HTTP
   test. Retain test containers/data for inspection; cleanup is separate.

MySQL schema/race checks:

```sh
SUMMARY_CONTENT_MYSQL_TEST_DSN='root@tcp(127.0.0.1:28306)/summary_versioning_test?parseTime=true' \
CGO_ENABLED=0 go test ./internal/service -run TestContentMySQL -count=1 -v
```

The tests create uniquely named disposable databases. They verify migration
replay, unique-index arbitration, transactional edit/restore, runtime admission/
CAS, duplicate completion, audit-failure rollback, sticky enrollment, concurrent
legacy-writer/cleaner fencing and deletion cancellation. Old scheduled requeue
is fenced, not yet replaced by coordinated execution. Production command routes
remain deliberately unmounted.

## Runtime test image

This image runs Go test binaries, not a user-facing API/Web app. The build context
excludes Git metadata, checkpoints and environment files. It calls no external
LLM or notification service.

```sh
docker build -f tests/content-contract-mirror/Dockerfile.runtime-tests \
  -t octo-summary-runtime-tests:legacy-fence-20260908 .
docker run --name octo-summary-legacy-fence-tests-20260908 --read-only \
  --network octo-summary-versioning-test \
  -e 'SUMMARY_CONTENT_MYSQL_TEST_DSN=root@tcp(octo-summary-versioning-test-mysql:3306)/summary_versioning_test?parseTime=true&loc=Asia%2FShanghai' \
  octo-summary-runtime-tests:legacy-fence-20260908
docker run --name octo-summary-content-worker-tests-20260908 --read-only \
  --network octo-summary-versioning-test \
  -e 'SUMMARY_CONTENT_MYSQL_TEST_DSN=root@tcp(octo-summary-versioning-test-mysql:3306)/summary_versioning_test?parseTime=true&loc=Asia%2FShanghai' \
  --entrypoint /bin/content-worker-tests \
  octo-summary-runtime-tests:legacy-fence-20260908 -test.run TestContentWorker -test.v
```

Both named containers are retained after exit. Inspect their recorded results:

```sh
docker logs octo-summary-legacy-fence-tests-20260908
docker logs octo-summary-content-worker-tests-20260908
```

Use a new explicit container name for another run; do not replace or delete mirror
containers. The empty root password belongs only to the isolated disposable DB
described above.

The Worker tests use synthetic model callbacks and the real durable poller.
They cover startup discovery without an HTTP wakeup, write-allowlist isolation,
expired-lease restart recovery, saturated-pool retry and MySQL legacy failure
accounting. They send no notifications and perform no external retrieval.
Keep `SUMMARY_CONTENT_WRITE_SPACES` unset on normal environments: full
generation, scheduling and UI adapters are still incomplete.
