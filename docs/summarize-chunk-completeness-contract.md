# Map completeness contract — `summarize_chunk` (#241 item 2 / #256)

This document is the ratified contract for the agent Map phase. Item-2 code in
`internal/agent/tool_summarize_chunk.go` (and its interaction with the
request-scoped `summaryHandleStore` and the runner completeness gates) must
conform to it; review this document, not a diff, when the behaviour is in
question. It exists because #241 scoped item 2 as "needs a design decision …
discuss before coding", and eight incremental rounds argued about *disclosure*
without pinning *which enforcement a tolerated gap replaced* — the omission that
produced the P0 in round 9.

## 0. Invariant

A run must never ship an **incomplete** summary as if it were **complete**.
"Complete" = every message the tool was asked to summarize is represented in the
Map output that feeds Reduce, OR the incompleteness is (a) recovered by a retry,
or (b) intentional and surfaced to the user through a channel the model cannot
silently drop.

## 1. Failure taxonomy

`summarize_chunk` fans a `messages_handle` into N chunks and summarizes each.
Each chunk ends in exactly one class:

| Class | Cause | Recoverable by retry? |
|---|---|---|
| **success** | non-empty summary | n/a |
| **blank-success** | call succeeded, summary whitespace-only (the prompt tells the model to skip off-topic chatter) | n/a — the messages were considered, nothing was worth reporting |
| **transient failure** | 429 / 5xx / network / per-attempt timeout / recovered panic | **yes** |
| **fatal failure** | output truncated, reasoning-budget exhausted, `ErrRequestTooLarge` | **no** — the same input can't shrink/complete on retry |
| **cap drop** | chunk beyond `maxChunkCalls` (256), removed by `capChunks` | **no** — retrying re-hits the cap |
| **manifest-miss** | message fetched after the citation manifest froze (V2 only) | **no** — not citable under this run |

## 2. The central decision: recovery-first

A transient per-chunk failure is **recovered by a retry**, not tolerated-and-shipped
as a partial. This is the pivotal choice (option X): a transient blip on one of up
to 256 chunk calls is the common case, and retrying yields a **complete** summary
with no reliance on a disclosure the model might drop. Only genuinely
**unrecoverable-by-retry** gaps — the intentional fan-out **cap**, or a **fatal**
error — produce a non-complete outcome, and the cap's is disclosed.

The rejected alternative (option Y: keep shipping the partial and guarantee a
model-proof warning in every config) was declined because it keeps handing the
user an incomplete summary and requires the #267 merge-channel work; and the
reviewer's hybrid ("keep the partial summary AND owe a retry") buys nothing here:
a retry re-runs the whole invocation over the same `messages_handle` so the
successful chunks are recomputed anyway, and `summaryHandleStore.ResolveAllBefore`
requires Reduce to consume *every* handle in the store, so a retained partial
handle plus a retry handle would force a double-merge. A transient-failed
invocation therefore does **not** persist an independently-mergeable partial
handle; it returns an error and owes a retry.

## 3. Enforcement authority per class

Which mechanism is authoritative, and whether it depends on `AGENT_SUMMARY_V2_MODE`:

| Class | Authoritative mechanism | V2-dependent? | Outcome |
|---|---|---|---|
| **transient failure** | **request-scoped Map-retry gate** (`MarkMapFailed` → `PendingMapFailures` → Reduce blocked + final-answer fail-closed) | **NO — flag-independent** | The tool returns an error; the invocation **owes a successful retry**. Reduce/final-answer blocked until every failed Map invocation is retried, or the run fails closed at the step ceiling. Never silently shipped. |
| **fatal failure** | abort the phase (`isFatalChunkError` → tool errors) | no | Invocation fails; `classifyToolError` maps it (`REQUEST_TOO_LARGE` = fatal, non-retryable for a critical tool). |
| **cap drop** | disclosure (`ChunkCallsCapped` + `CappedDroppedCount` + inline notice; V2: `PARTIAL`) | disclosure V2-gated | Intentional, **not** a retry obligation. Shipped as a disclosed partial. Model-proof `V2=off` disclosure = **#267**. |
| **blank-success** | none — **not a gap** | n/a | Counted as **processed** (covered) and in `BlankChunkCount` for observability; **not** loss, does **not** assert incompleteness. The code cannot distinguish "model found nothing" from "gateway ate the response"; per r9 the false-incompleteness (crying wolf) is the worse failure, so blank is treated as covered. |
| **manifest-miss** | disclosure (`DroppedCount`; V2: `PARTIAL`) | disclosure V2-gated | Accidental loss; disclosed. |

## 4. Coverage field semantics (`chunkCoverage` / tool result)

| Field | Meaning |
|---|---|
| `input_count` | messages handed to the invocation (before manifest filtering) |
| `processed_count` | messages in success **and blank-success** chunks (both covered) |
| `dropped_count` | accidental loss = manifest-miss (transient is retried not shipped; blank is covered; cap is separate) |
| `capped_dropped_count` | messages removed by the fan-out cap (only when it fired) |
| `blank_chunk_count` | blank-success chunks — observability, **not** loss |
| `failed_chunk_count` | chunks that hit a transient failure. Because any transient failure makes the tool return an error, a **returned** result always has this = 0; retained for the error/log path |
| `oversized_message_count` | messages over `oversizedMessageRunes`, across all formatted chunks |
| `chunk_calls_capped` | the fan-out cap fired |
| `truncated` | any real gap = `dropped_count > 0 || capped_dropped_count > 0` (blank excluded) |
| `chunk_size` | resolved per-chunk message cap |

## 5. Cap semantics

- `capChunks` keeps the **most recent** `maxChunkCalls` (256) chunks (tail slice); the **oldest head** is dropped (pool is time-ascending).
- Intentional and disclosed; counted in `capped_dropped_count` (only when fired) and `chunk_calls_capped`; **not** a retry obligation.
- Scope is **per-invocation**. A per-request bound (a run can issue up to `maxSummaryHandles`=128 invocations) is a follow-up, not in this contract.

## 6. Disclosure termination per `AGENT_SUMMARY_V2_MODE`

| State | Structural channel (model-proof) | In-text notice (model-rewritable) |
|---|---|---|
| `off` (default) | **none** — no run row; `recordDroppedMessages` no-ops | `mapCoverageGapNotice` appended to Map output (best-effort) |
| `shadow` / `on` | `recordDroppedMessages` → finishgate `PARTIAL` → `finish_status` | same notice, second layer |

For **transient failures** this asymmetry is irrelevant — they are gated by the
flag-independent retry gate (§3). It matters only for **cap / manifest-miss**
(unrecoverable, disclosed) at `V2=off`, where the model-proof channel is #267.

## 7. Required code (the #256 rework)

1. **P0** — a transient per-chunk failure returns an error (the failure guard
   fires on *any* failure, not only all-failed), so the runner `MarkMapFailed`s
   the invocation (flag-independent) and no partial handle is stored. Pin: a
   stubbed 1-of-N-chunk 429 returns an error, not a partial, at concurrency 1 & N.
2. **P1** — `ErrRequestTooLarge` ∈ `isFatalChunkError`. **DONE** (`745bbd9`).
3. **blank** — count blank-success as processed (covered); keep it out of
   `dropped_count`/`truncated`. Pin: an all-success run with one blank chunk has
   `truncated=false`.
4. **eval** — the SS-02 gate label reflects disclosed-to-user loss; a gap must
   survive Reduce + final answer.
5. **cap** — per-invocation scope stated explicitly (schema/PR); per-request
   budget is a follow-up.

## 8. Out of scope (tracked elsewhere)

- **#267** — model-proof disclosure of the cap / manifest-miss gap at `V2=off`.
- Per-request fan-out budget (§5).
