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

## Input origin and routing

- `action: "chat"` with `input_origin: "user"` is conversational. Except for
  explanation-only turns, it always runs through the Agent and returns an
  `agent_preview` or `agent_revision`; it does not directly start a personal
  Workflow or create a team proposal.
- `input_origin: "template"` and `input_origin: "system_intent"` are trusted UI
  inputs. When the remaining route requirements are satisfied, they may enter
  the deterministic personal or team Workflow paths directly.
- `action: "start_team_workflow"` remains the explicit trusted team execution
  entry point and is not changed by the conversational routing rule above.

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
message retrieval, `scope_json`, and `preview.effective_scope`. Ordinary edits,
questions, negations, and historical references do not call the scope tool and
therefore keep the picker scope. On a cold start, a preview without a successful
source declaration becomes a clarification instead of an ungrounded preview or
a retryable server error. Because every user-authored non-explanation chat turn
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
