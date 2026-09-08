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

The tests create uniquely named disposable databases. They verify real MySQL
migration replay and unique-index arbitration; they do not yet prove runtime
lease recovery, coordinated legacy writers or output CAS.
