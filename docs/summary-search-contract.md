# S14 F6 task search (BE-2 / BE-3)

`GET /api/v1/summaries` (also the existing Bot list route) keeps its current
authentication, Space visibility, report permissions, filters and pagination
envelope. No frontend changes are required. The normalized search fields require the schema migration and Go backfill described below.

## Keyword contract

- Maximum 100 Unicode code points in the supplied `keyword`; overflow returns
  HTTP 400 / business code 40000. Input is never silently truncated.
- Trim query-edge whitespace, fold letter case and full/half width. Match a
  literal consecutive substring of **either** `title` or the displayed `topic`
  (`DisplayTopic()`, including its existing title fallback). Keep internal spaces
  and punctuation. `%`, `_`, quotes and backslashes are literal characters.
- Fields are independent; no concatenation, pinyin, traditional/simplified
  conversion, report-body, generation-instruction or hidden-source search.
- Empty/whitespace-only queries restore the normal list, with no search metadata.

For a nonempty keyword, each returned item adds:

```json
{
  "task_id": 123,
  "title": "周会排期",
  "topic": "方案评审",
  "created_at": "2026-10-12T08:00:00Z",
  "search_match": {"title_ranges": [], "topic_ranges": [[0, 4]]},
  "is_historical_match": true
}
```

Ranges are half-open **Unicode code-point offsets into the displayed original**,
not UTF-8 bytes or JavaScript UTF-16 code units. All hit positions are represented;
overlapping/adjacent ranges are merged. The item `task_id` is the matched run's
existing identity; `created_at` remains that run's date. `is_historical_match`
means a newer, non-deleted run in the same schedule is visible to this caller,
independent of keyword/status filters. No invisible run identity is returned.

## Fold, count and order

1. Apply the existing Space/user visibility and business filters, then match.
2. Fold by schedule ID, retaining the highest task ID among matching candidates.
   Manual tasks each retain their own identity (separate namespace).
3. Order by one exclusive priority: pending invitation, pending submission,
   unread, other. Multiple flags assign a task to the earliest group only.
4. Within a group: existing `activity_at` descending (team/personal generation
   time, falling back to creation time), creation time descending, task ID
   descending. Legacy `sort_by`/`sort_order` no longer override this fixed order.
5. `total` counts folded matches before pagination. Navigation attention counts
   retain their existing Space-wide, keyword-independent meaning.

## Storage, query cost and rollout

`summary_task.search_title` and `search_topic` persist the same Go width/case
normalization used for highlights. GORM task create/update hooks maintain them in
the same write as title/topic (including map updates, Save and selected fields).
Blank topics store an empty search value: title already covers the display fallback.
Fields use TEXT to accommodate case-fold expansion and are not exposed in JSON.
MySQL uses binary `LOCATE`; SQLite uses `instr`. SQL collation, accent equivalence,
and LIKE wildcards cannot change the match semantics.

The request no longer fetches every candidate's title/topic, normalizes them in
Go or transfers an unbounded matching-ID JSON array back to MySQL. Only one page
and its total reach the application. A CTE statement applies visibility/business
filters and matching, folds runs, and computes total/page/historical metadata in
one database snapshot. The window/CTE materializes only task IDs and ranks, not
full task rows or normalized text. Empty and out-of-range pages retain the correct total.
There is no handler-owned repeatable-read transaction. Existing enrichment and
Space-wide attention counters keep their original paths.

Literal arbitrary-substring matching still examines candidate normalized text
inside the database; this is **not** an indexed substring-search implementation.
The improvement removes per-request application normalization, bulk text transfer
and ID materialization. Representative production volume/concurrency benchmarks
are still necessary. Ordinary B-tree indexes do not accelerate an interior substring.

Deploy migration `20261021-01-summary-search-normalized.sql` together with the new
API and worker binaries. `RunMigrations` performs an idempotent Go backfill under
the migration lock, in transactions of at most 500 rows, before either binary
serves work. Row locks protect against concurrent new writers, and failures stop
startup so incomplete search data is not served. Backfill explicitly self-assigns
`updated_at` to suppress MySQL's `ON UPDATE CURRENT_TIMESTAMP`, preserving historical
business timestamps. The MySQL regression uses fixed 2019/2020 timestamps across
501 rows so same-second fixture creation cannot hide an accidental rewrite.
Stop old API/worker writers before this upgrade: old binaries/raw SQL and GORM
`SkipHooks`/`UpdateColumns` do not maintain the new fields. Do not use a rolling
mixed-version write deployment. If `SKIP_MIGRATION=true`, provision both schema
and Go backfill before startup; SQL LOWER cannot replace this normalization.

## Concurrent permission changes

Existing visibility and detail authorization predicates are unchanged. A revoke
or soft delete committed before the list statement is excluded from both count
and page. A change committed after that statement's snapshot may leave the
already-in-flight response showing the prior item; subsequent list/detail/result
requests honor the committed change (403 for lost membership, 404 for deletion).
This is ordinary statement snapshot behavior, with no application scan extending
an earlier permission snapshot across multiple queries.

## Verification

Pure matching tests run with or without CGO. SQLite integration tests cover
matching metadata, permissions-as-existing, folding, filtering, 205-result
pagination, exclusive priority and time/identity tie-breakers. The same scenarios
run against MySQL 8 when `SUMMARY_MYSQL_TEST_DSN` points to an isolated, migrated
test database, plus config-only schedule invitees, concurrent title edits, revoke/delete before
and after the list statement, and restartable 501-row backfill. Model tests cover
create, map/struct updates, selected fields and Save. Existing authorization tests
remain intact.

## Local performance probe (2026-10-21)

An isolated MySQL 8 probe used 10,000 visible manual tasks, each with a 20-character
Chinese title and 1,000-character Chinese topic, and an absent keyword. Comparing
the old metadata-transfer/Go-normalization stage with the new database predicate:

| Matching stage | Elapsed | Application cumulative allocations | Candidate text transferred to app |
| --- | ---: | ---: | ---: |
| Old application scan | 216 ms | 40.74 MiB | 30,600,000 bytes |
| Persisted fields / DB match | 64 ms | 0.01 MiB | 0 bytes |

This is a single local matching-stage probe, not an end-to-end API benchmark or
production p95; it excludes fixture creation and does not measure high-hit-rate
folding/sorting or concurrent production traffic. Both stages returned zero hits.
