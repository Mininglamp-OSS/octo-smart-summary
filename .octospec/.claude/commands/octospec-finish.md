---
description: octospec Finish — final check, record journal + learnings, open a PR
argument-hint: <slug>
---

You are running the octospec **Finish** phase for task `$ARGUMENTS`.

1. Run the final verification gate one more time (lint / type-check / tests).
2. Write `.octospec/journal/shared/$ARGUMENTS.md` — a short, team-visible record:
   what was done, any structural learning, any gotcha worth remembering. Start
   the file with OKF frontmatter (`type: Journal`, plus `title`/`description`/
   `tags`/`timestamp`) so it stays a valid OKF unit and passes `octospec-lint`.
   Also add a dated entry to `.octospec/log.md` (create it if missing).
3. **Learning promotion (in-place write-back, same PR).** If this task produced a
   reusable learning, land it **in this same PR** — do NOT stage it in
   `learnings/pending/` for a separate PR or a later move (that PR never happens,
   so the learning stays stranded on the feature branch). Two cases:
   - **Existing rule applies** → edit the relevant `.octospec/rules/<rule>.md`
     in place (add/refine a bullet), bump its `timestamp`, and note the change in
     the journal. The PR review IS the gate for the rule edit.
   - **No rule fits yet** → create a new `.octospec/rules/<slug>.md` with full
     OKF frontmatter (`type: Rule`, `title`, `description`, `inject_when`,
     `priority`, `load_bearing`, `tier`, `source`, `timestamp`) and add it to
     `rules/_index.yaml`.
   The helper `.octospec/scripts/octospec-update-spec.sh` gives you the raw material for
   this — `--kind=rule` writes a rule draft to `learnings/pending/<slug>-rule-draft.md`
   plus a promotion block (proposed body + COMPREHENSION questions) on stdout;
   `--kind=task` writes a per-actor journal entry. The helper never auto-writes
   `rules/` (that keeps the comprehension gate human), so **you** copy the draft
   into `rules/<id>.md` and update `_index.yaml` here, in this PR — then drop the
   scratch draft. The draft is in-PR scratch, not a deliverable to promote later.
   Only leave something in `learnings/pending/` when the learning is genuinely
   *unresolved* (needs human design before it can become a rule) — pending is for
   open questions, not for finished learnings awaiting a phantom review PR.
4. Open a PR. Pre-fill the body using `.github/PULL_REQUEST_TEMPLATE.md`:
   - **Linked Spec** → `.octospec/tasks/$ARGUMENTS/brief.md`
   - **COMPREHENSION** three questions answered to substance (for load-bearing /
     architectural / P0 changes; omit for trivial ones).
5. Commit with a Conventional Commit message referencing the issue.
