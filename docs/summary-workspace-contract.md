# Summary workspace contract v2

The summary workbench is available when `GET /summary-workbench/capabilities`
returns `enabled: true` and `contract_version: "2"`.

## Direct team workflow

- `direct_team_workflow: true` means the client may send
  `action: "start_team_workflow"` instead of showing a confirmation card.
- This capability currently ships together with the v1 workbench; it is not a
  separate rollout flag.
- The explicit action is treated as the user's execution intent. The server
  still requires a valid team scope and requirement, and revalidates source,
  participant, and reference permissions before creating the workflow.
- Reusing the same `request_id` is idempotent and does not dispatch a second
  workflow.

## Mixed sources (document + chat)

- `mixed_sources: true` means the server admits a document+chat source scope
  (both the workspace and legacy HTTP create paths) and the client may offer
  mixed selection. Mixed admission is unconditional (the phase-1 roll-out gate
  was removed when the worker executor landed), so the field is always `true`;
  the client feature-detects on it rather than on `contract_version`, which is
  intentionally not bumped for this additive optional field.
- A mixed scope is personal-only (no participants, no origin auto-reply) and
  does not support stacking a referenced summary.
- The document executor does not consume referenced summaries. A document-only
  scope containing references returns a recoverable clarification (HTTP 200),
  retaining the reference chips and creating no task or document fetch. This
  also lets older persisted scopes round-trip instead of getting stuck on 400.
  Remove the reference chips before execution; mixed document+chat+reference
  submissions retain their existing HTTP 400 validation.

## Input origin and routing

- For **chat-only scopes**, `action: "chat"` with `input_origin: "user"` is conversational. Except for
  explanation-only turns, it always runs through the Agent and returns an
  `agent_preview` or `agent_revision`; it does not directly start a personal
  Workflow or create a team proposal.
- `input_origin: "template"` is inferred or retained when the message matches
  the selected template requirement (trimmed, case-insensitive).
  An explicit `input_origin: "system_intent"` is retained for a complete
  supported execution command, using the same command list as the execute path.
  With no origin declaration, only the four legacy Chinese/English button
  labels infer `system_intent`; typed aliases such as `start summary` retain
  their existing conversational behavior in chat-only scopes.
  Known origin/content mismatches downgrade to `user`, rather than rejecting
  localized copy changes with HTTP 400. Unknown origin enum values still return
  HTTP 400. Origin metadata alone is not proof of a registered template.
  A recognized system message is retained as the document-summary requirement
  when no template is selected; chat-only requirement handling is unchanged.
  When the remaining route requirements are satisfied, recognized origins may
  enter deterministic personal or team Workflow paths directly.
- **Document-bearing scopes:** participants are unsupported, unavailable
  selected chats block execution. Negative-action analysis has three outcomes:
  no blocking negation, recognized cancellation, and uncertain negative intent.
  Both of the latter outcomes clarify before any task or document fetch.
  A conflicting preview request does not override that decision:
  `不要生成正式总结，先给预览` now clarifies rather than generating/revising
  a preview. Positive preview requests and revisions without blocking negative
  intent are unchanged. Unknown negative content restrictions also clarify on
  chat-only previews; they do not fall through to revision.
  Known negative verbs are found anywhere except positively attributed quotations; there
  is no discourse-prefix or whole-scope-object allowlist. An unrecognized object
  after such a verb defaults to clarification, not execution. Thus `算了不要总结`,
  `不要总结所有内容`, `不要总结这份文档` and new unknown objects all stop.
  This deliberate product trade-off may ask an extra question for an unfamiliar
  content restriction; it is not a claim of complete natural-language understanding.
  Recognized whole content/output objects (tables, action items, attachment
  content, closed tasks and the tested variants) do not by themselves cancel:
  `总结一下，不要生成表格`, `不需要总结附件内容` and
  `总结这份文档，表格不用生成` do not cancel. Pre- and post-verbal
  objects use the same whole-object classifier; conjunctions (`和`, `与`,
  `以及`, `、`, English `and`) are allowed only when every member is known.
  `、` stays inside the object rather than hiding a later unknown member.
  Matching is not a substring exemption: `不要生成表格和总结`,
  `不要生成表格、总结` and `未知表格不用生成` clarify.
  For a same-clause affirmative request followed by a fronted object, preserved
  whitespace is required as a boundary, and the preceding request must be one
  of the recognized forms in `hasFrontedContentObject` (for example,
  `总结这份文档 表格不用生成`). Unrecognized request/topic constructions
  remain conservative; no arbitrary noun-suffix extraction is performed.
  A separator directly between a fronted object and a negative action is not
  erased to authorize execution: `表格、不要总结` and `表格 不要总结`
  clarify conservatively, while the established `表格不用生成` form remains
  a content constraint. This is a regression guard, not additional grammar.
  Content members are checked incrementally against the same finite object
  set; an impossible member fails early without splitting every remaining
  suffix. Long lists composed entirely of known members are not length-capped.
  Meaning questions such as `不要生成总结是什么意思` are not cancellation.
  The reply asks whether the user means cancellation or a content restriction
  and suggests restating the requirement positively; it does not assert intent.
  Quote spans retain object text. Quotation alone does not suppress a negative verb:
  `不要总结“这些内容”` clarifies; `不要总结“附件内容”` is a known content
  restriction; reported `文档写着“不要总结”` is not a cancellation instruction.
  Explicit local attribution markers (such as `写着`, `他说：`, `内容是`,
  or English `said`) suppress reported negative verbs; meaning questions remain
  independently exempt. Unattributed quoted negative instructions are analyzed
  even with punctuation, brackets or politeness outside the quotes:
  `“不要总结”。`, `“不要总结”，谢谢` and `（“不要总结”）` clarify.
  An affirmative request elsewhere does not override a conflicting quoted
  cancellation. Unknown attribution wording can ask for confirmation.
  Empty quote pairs do not discard the surrounding clause. An entirely quoted
  submission is the user's instruction. A definite cancellation takes precedence
  over uncertain negation regardless of clause order; both still block execution.
  Stack-based quote pairing is linear,
  including unmatched/nested openers. Rich-text U+FFFC placeholders are ignored.
  CJK inter-character whitespace, English whitespace runs, in-word smart
  apostrophes, common trailing particles and politeness tails are normalized.
  This parser only analyzes negative intent; affirmative quoted-text behavior
  remains unchanged and carries no new execution-authorization guarantee. Otherwise,
  document+chat submissions start a personal Workflow even when phrased as
  questions. This mixed-question execution behavior is retained for compatibility.
  Documents-only `Explain` input clarifies unless the template submission is
  verified against the server registry: the built-in pattern or the current
  actor/space's non-deleted custom/overridden pattern must match the submitted
  requirement case-insensitively. A fabricated ID, forged content, another
  actor's template, or stale override cannot widen this route. Registry lookup
  errors fail before turn creation. On documents-only scopes, a submission
  consisting entirely of recognized content restrictions (for example,
  `不要生成表格`, `不需要总结附件内容`, or `表格不用生成`) also clarifies
  unless it is a server-verified template submission. Not being a cancellation
  is not by itself permission to start a document task. This narrow guard uses
  the existing content-object vocabulary; it is not a new affirmative-phrase
  requirement for all document requests. `总结一下，不要生成表格` executes:
  this is an intentional widening from the base's substring cancellation guard,
  not an unchanged document-only behavior. Free-text requirements retain their
  existing route. Mixed-scope submissions retain the compatibility rule above,
  including known content restrictions alone. Unknown negative objects still
  hit the earlier clarification guard, even with a verified template.
  A run phrase's position in user text does not override this rule.
- Mixed conversational question-answering is not implemented by this contract.
  Separating it from execution would require a future structured personal
  execution action, which is not currently exposed.
- The shared classifier also changes chat-only content constraints such as
  `不要生成表格` from Explain to Generate. User-authored turns still use the
  conversational Agent preview path; matching template submissions can execute
  when the normal route requirements hold. A non-command `system_intent`
  declaration downgrades to user. This is a classification compatibility
  change, not additional dispatch authority, and is covered separately.
- `action: "start_team_workflow"` remains the explicit trusted team execution
  entry point and is not changed by the conversational routing rule above.
  Its existing chat-only route does not consult this negative-intent guard;
  the zero-task cancellation guarantee above does not cover that explicit
  action. Closing that pre-existing gap requires a separate scoped change.

## Effective scope

The session's `scope_json` and `scope_hash` are the authoritative effective
scope. When the server resolves a default, inferred source, or explicit
natural-language source/time-range change, the completed turn stores that
resolved scope without changing `scope_version`. The response returns the same
scope in `state.summary_context`; the client must use it for the next turn and
must replace its local chat chips with the returned `selected_channels`.

Conversational source replacement/extension is two-phase. The Agent first
resolves candidate chats through server-authorised discovery, then calls
`set_summary_scope` with one final `keep`, `replace`, or `extend` decision. The
server accepts only discovered channels and applies the 30-chat limit, actor
membership, and team-scope checks before atomically replacing the stored scope.
Discovery only creates candidates: a newly discovered chat cannot be read until
the declaration succeeds. The declaration is single-use for the turn, freezes
further discovery, and its complete final channel list is the one used for
message retrieval, `scope_json`, and `preview.effective_scope`. Explanation-only
questions, recognized cancellations and uncertain negative intent keep the
Agent scope locked. Cancellation analysis precedes preview revision, and an
execution-command prefix cannot override it when a preview exists.
Content constraints such as "do not generate a table" may still enter the
conversational Agent path; that permission is not evidence of a scope change.
Only a separately authorized source/time-range declaration may change the picker
scope. On a cold start, a preview without a successful
source declaration becomes a clarification instead of an ungrounded preview or
a retryable server error. For chat-only scopes, every user-authored non-explanation chat turn
is conversational, it cannot directly dispatch a Workflow. A trusted
template/system intent or the explicit team start action may do so after the
resolved scope is returned.

`time_range.source` records whether the current range came from the picker,
the server default, or a conversational instruction. Explicit conversational
ranges, including non-preset ranges such as three days or two weeks, are parsed
by the Agent and declared through `set_summary_scope` before message retrieval.
When no range is selected, the server materializes a fixed 30-day window and
labels it `最近 30 天（默认）`.
The tool rejects inverted ranges, ranges over 90 days, and labels over 256
characters before any retrieval starts. Incidental, questioned, negated,
complained-about, or historical mentions do not replace the current range.

Each preview message is bound to the exact Agent run that generated it. Saving
the preview first resolves citations from that run's evidence session. An
evidence-free `agent_revision` may inherit citations only through its explicit
`parent_message_id` preview chain; replacement/extension runs with their own
evidence remain authoritative. If the workspace scope references an existing
summary, the save path may instead borrow that referenced artifact's citations,
or remove its now-unresolvable markers when citation details are unavailable.
The workspace session identity is used only as a compatibility fallback for
legacy messages without a persisted run binding.

If a `[1]`-anchored citation sequence cannot be resolved from the generating
run, its preview ancestors, or a referenced artifact, save returns HTTP 409 with
`code: 40902`, `reason: "workspace_citation_unresolved"`, and
`recovery_action: "regenerate_preview"`. Reloading the unchanged session is not
expected to repair this condition.

A pending team proposal also stores its resolved `time_range`. Confirmation
uses that stored value, so the displayed range and the dispatched workflow
window cannot diverge.

## History preview field

History messages for `agent_preview` and `agent_revision` may include an
additive `preview` object. It contains the immutable content and version
metadata for that historical message. Only the artifact referenced by current
state exposes actions; older previews are read-only. Clients that ignore
unknown fields remain compatible with contract v1.

## Regression validation

Run both `CGO_ENABLED=0 go test ./...` and `CGO_ENABLED=1 go test ./...`.
The real handler/document-source and template-registry tests require CGO;
a green no-CGO suite alone does not exercise those guards. Include the
handler race tests when changing routing or workflow creation.
