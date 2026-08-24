#!/usr/bin/env bash
# octospec-update-spec — Finish-phase learning reflow tool.
#
# Turns a single task's learning into the right artifact BY KIND. The script
# never auto-writes main's rules/ — that safety floor keeps the comprehension
# gate human — but it produces the material the author uses to land the rule
# IN THE SAME PR. This is the executable backing for the parity design §3
# (learning reflow) / §6 (task context).
#
# Two reflow paths, selected with --kind:
#
#   --kind=rule  (規範級 / rule-level)
#     A learning that should constrain EVERY future task. The script:
#       1. writes a DRAFT OKF Rule (full frontmatter + octospec extension fields)
#          to  .octospec/learnings/pending/<slug>-rule-draft.md
#          — pending/ here is scratch material for THIS PR, not a dead-letter to
#            be promoted in some future PR;
#       2. prints promotion material to stdout (proposed rule body + COMPREHENSION
#          three questions + a checklist) so the AUTHOR can, in this same PR,
#          land the rule into .octospec/rules/<id>.md and add the
#          rules/_index.yaml entry. The PR review is the comprehension gate.
#     The script writes the draft + material; the author (a human, in this PR)
#     does the rules/ landing. The script itself never edits rules/ or
#     rules/_index.yaml — that is the only step it leaves to the author, and it
#     happens in the same PR, never a separate one.
#
#   --kind=task  (任務級 / task-level)
#     A reusable, actor-scoped learning that should be committed with the work,
#     not enforced on everyone. The script writes a per-actor journal entry to
#       .octospec/journal/by-actor/<actor>/<slug>.md
#     with OKF `type: Journal` frontmatter. It stays IN THE REPO — octospec is
#     self-contained and never writes to any external/personal memory service.
#
# Idempotency: rerunning the same slug+kind does not pile up files. By default the
# rule draft / journal entry is OVERWRITTEN in place; with --skip-existing an
# existing file is left untouched.
#
# No hard external dependencies. Style mirrors octospec-sync.sh /
# octospec_sync_block.py (set -euo pipefail, atomic writes, explicit refusals).
#
# Usage:
#   octospec-update-spec.sh --slug <slug> --kind rule|task [--learning <text>] [opts]
#   echo "<learning text>" | octospec-update-spec.sh --slug <slug> --kind rule
#
# Common options:
#   --slug <slug>            (required) kebab-case task slug.
#   --kind rule|task         (required) reflow path.
#   --learning <text>        Learning text. If omitted, read from stdin.
#   --skip-existing          do not overwrite an existing draft / journal entry.
#   --no-promote             rule: write the draft but suppress the promotion
#                            material on stdout (escape hatch to avoid noise).
#
# Rule-only options (sane defaults; the human refines in review):
#   --title <title>          Rule title (default: derived from slug).
#   --description <text>     Rule description (default: first line of learning).
#   --tags <a,b,c>           OKF tags (default: from --inject-touches or slug).
#   --rule-id <id>           Rule id (default: <slug>).
#   --tier repo|global       (default: repo).
#   --priority <0-100>       (default: 50).
#   --load-bearing           Mark load_bearing: true (default: false).
#   --inject-paths <g,...>   inject_when.paths globs (default: ["**"]).
#   --inject-touches <t,...> inject_when.touches tags (default: []).
#
# Task-only options:
#   --actor <name>           Actor handle for the journal lane (default: derived
#                            from `git config user.name`, lowercased to [a-z0-9-]).
#   --tags <a,b,c>           OKF Journal tags (default: ["octospec-learning","<slug>"]).
#   --title <title>          Journal title (default: derived from slug).
#
# Exit codes: 0 ok; 2 = usage error / refusal.
set -euo pipefail

PROG="octospec-update-spec"

die() { echo "$PROG: $*" >&2; exit 2; }

usage() {
  sed -n '2,/^set -euo pipefail$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; s/^#$//'
}

# --- locate the .octospec dir (works from anywhere inside the repo) -----------
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Respect an explicitly provided OCTOSPEC_DIR (the script ships as a template and
# is invoked against a target repo's .octospec/, which is NOT the script's own
# parent once installed). Fall back to scripts/.. only when unset.
OCTOSPEC_DIR="${OCTOSPEC_DIR:-$(cd "$HERE/.." && pwd)}"   # scripts/ -> .octospec/
PENDING_DIR="$OCTOSPEC_DIR/learnings/pending"
BY_ACTOR_DIR="$OCTOSPEC_DIR/journal/by-actor"

# --- defaults -----------------------------------------------------------------
SLUG=""
KIND=""
LEARNING=""
LEARNING_SET=0
SKIP_EXISTING=0
NO_PROMOTE=0
TITLE=""
DESCRIPTION=""
TAGS=""
RULE_ID=""
TIER="repo"
PRIORITY="50"
LOAD_BEARING="false"
INJECT_PATHS=""
INJECT_TOUCHES=""
ACTOR=""

# --- arg parsing (supports `--opt val` and `--opt=val`) -----------------------
while [ $# -gt 0 ]; do
  arg="$1"
  val=""
  case "$arg" in
    --*=*) val="${arg#*=}"; arg="${arg%%=*}";;
  esac
  takeval() {
    if [ -n "$val" ]; then printf '%s' "$val"; return 0; fi
    [ $# -ge 2 ] || die "option $1 needs a value"
    # Reject the next token when it is itself a long option (--foo): otherwise a
    # mistyped `--title --priority 88` silently swallows `--priority` as the
    # title. The `--opt=val` form sets $val above and never reaches here, so
    # values that legitimately start with a single dash still work via `--opt=-x`.
    case "$2" in
      --*) die "option $1 needs a value (got the option '$2'); use $1=<value> if the value really starts with --";;
    esac
    printf '%s' "$2"
  }
  case "$arg" in
    -h|--help) usage; exit 0;;
    --slug)          SLUG="$(takeval "$@")"; [ -n "$val" ] || shift;;
    --kind)          KIND="$(takeval "$@")"; [ -n "$val" ] || shift;;
    --learning)      LEARNING="$(takeval "$@")"; LEARNING_SET=1; [ -n "$val" ] || shift;;
    --title)         TITLE="$(takeval "$@")"; [ -n "$val" ] || shift;;
    --description)   DESCRIPTION="$(takeval "$@")"; [ -n "$val" ] || shift;;
    --tags)          TAGS="$(takeval "$@")"; [ -n "$val" ] || shift;;
    --rule-id)       RULE_ID="$(takeval "$@")"; [ -n "$val" ] || shift;;
    --tier)          TIER="$(takeval "$@")"; [ -n "$val" ] || shift;;
    --priority)      PRIORITY="$(takeval "$@")"; [ -n "$val" ] || shift;;
    --inject-paths)  INJECT_PATHS="$(takeval "$@")"; [ -n "$val" ] || shift;;
    --inject-touches) INJECT_TOUCHES="$(takeval "$@")"; [ -n "$val" ] || shift;;
    --actor)         ACTOR="$(takeval "$@")"; [ -n "$val" ] || shift;;
    --load-bearing)  LOAD_BEARING="true";;
    --skip-existing) SKIP_EXISTING=1;;
    --no-promote)    NO_PROMOTE=1;;
    --) shift; break;;
    -*) die "unknown option: $arg (use --help)";;
    *) die "unexpected argument: $arg";;
  esac
  shift
done

# --- validate -----------------------------------------------------------------
[ -n "$SLUG" ] || die "--slug is required"
[ -n "$KIND" ] || die "--kind is required (rule|task)"
case "$KIND" in rule|task) :;; *) die "--kind must be 'rule' or 'task', got: $KIND";; esac
case "$SLUG" in
  [a-z]*) :;;
  *) die "--slug must be kebab-case, start with a letter: $SLUG";;
esac
case "$SLUG" in
  *[!a-z0-9-]*) die "--slug must match [a-z0-9-]: $SLUG";;
esac
case "$PRIORITY" in
  ''|*[!0-9]*) die "--priority must be an integer 0-100: $PRIORITY";;
  *) [ "$PRIORITY" -le 100 ] || die "--priority must be 0-100: $PRIORITY";;
esac
case "$TIER" in repo|global) :;; *) die "--tier must be repo|global: $TIER";; esac

# title/description land in a single-line double-quoted YAML scalar; a literal
# newline would split the scalar and emit malformed YAML. Fold embedded newlines
# (and carriage returns) to spaces so a multi-line value stays lint-clean.
TITLE="$(printf '%s' "$TITLE" | tr '\n\r' '  ')"
DESCRIPTION="$(printf '%s' "$DESCRIPTION" | tr '\n\r' '  ')"

# --- gather the learning text -------------------------------------------------
if [ "$LEARNING_SET" -eq 0 ]; then
  if [ -t 0 ]; then
    die "no learning text: pass --learning <text> or pipe it on stdin"
  fi
  LEARNING="$(cat)"
fi
# Trim leading/trailing blank lines but keep internal structure.
# Portable (awk, no GNU-sed extensions): buffer the lines, drop leading and
# trailing empty lines, print the rest verbatim. Works on BSD/macOS + Linux.
LEARNING="$(printf '%s\n' "$LEARNING" | awk '
  { line[NR] = $0 }
  END {
    s = 1;  while (s <= NR && line[s] == "") s++
    e = NR; while (e >= s  && line[e] == "") e--
    for (i = s; i <= e; i++) print line[i]
  }')"
[ -n "$LEARNING" ] || die "learning text is empty"

FIRST_LINE="$(printf '%s\n' "$LEARNING" | sed -n '1p')"
TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# --- helpers: slug -> Title Case; comma list -> YAML flow seq ------------------
titlecase_slug() {
  printf '%s' "$1" | tr '-' ' ' | awk '{for(i=1;i<=NF;i++){$i=toupper(substr($i,1,1)) substr($i,2)}; print}'
}

# Turn "a, b ,c" into a YAML/JSON-safe flow sequence: ["a","b","c"].
# Empty input yields []. Pathname expansion is disabled around the unquoted
# split so glob-like items (e.g. "modules/**/*.go") stay literal.
csv_to_flow_seq() {
  local csv="$1" out="" item first=1
  local IFS=','
  local restore_glob=1
  case "$-" in *f*) restore_glob=0;; esac
  set -f
  # shellcheck disable=SC2086  # intentional word-split on IFS=',' with glob off
  for item in $csv; do
    # trim surrounding whitespace
    item="${item#"${item%%[![:space:]]*}"}"
    item="${item%"${item##*[![:space:]]}"}"
    [ -n "$item" ] || continue
    # escape backslash and double-quote for safe quoting
    item="${item//\\/\\\\}"
    item="${item//\"/\\\"}"
    if [ "$first" -eq 1 ]; then out="\"$item\""; first=0; else out="$out, \"$item\""; fi
  done
  [ "$restore_glob" -eq 1 ] && set +f
  printf '[%s]' "$out"
}

# Atomic write: temp file in target dir + mv (mirrors octospec_sync_block.py).
atomic_write() {
  local target="$1" content="$2" dir tmp
  dir="$(dirname "$target")"
  mkdir -p "$dir"
  tmp="$dir/.$(basename "$target").$PROG.tmp"
  printf '%s' "$content" > "$tmp"
  mv -f "$tmp" "$target"
}

# ============================================================================
# kind=rule
# ============================================================================
if [ "$KIND" = "rule" ]; then
  [ -n "$RULE_ID" ] || RULE_ID="$SLUG"
  # RULE_ID becomes the YAML `id:` scalar and the rules/<id>.md filename, so hold
  # it to the same kebab-case shape as --slug. A raw value with `:` or `/` would
  # emit malformed frontmatter / a bad path; reject it rather than escape it.
  case "$RULE_ID" in
    [a-z]*) :;;
    *) die "--rule-id must be kebab-case, start with a letter: $RULE_ID";;
  esac
  case "$RULE_ID" in
    *[!a-z0-9-]*) die "--rule-id must match [a-z0-9-]: $RULE_ID";;
  esac
  if [ -z "$TITLE" ]; then TITLE="$(titlecase_slug "$SLUG")"; fi
  if [ -z "$DESCRIPTION" ]; then DESCRIPTION="$FIRST_LINE"; fi

  # tags default: inject-touches, else the slug.
  if [ -z "$TAGS" ]; then
    if [ -n "$INJECT_TOUCHES" ]; then TAGS="$INJECT_TOUCHES"; else TAGS="$SLUG"; fi
  fi
  TAGS_SEQ="$(csv_to_flow_seq "$TAGS")"
  TOUCHES_SEQ="$(csv_to_flow_seq "$INJECT_TOUCHES")"
  if [ -n "$INJECT_PATHS" ]; then
    PATHS_SEQ="$(csv_to_flow_seq "$INJECT_PATHS")"
  else
    PATHS_SEQ='["**"]'
  fi

  DRAFT_PATH="$PENDING_DIR/${SLUG}-rule-draft.md"

  if [ -e "$DRAFT_PATH" ] && [ "$SKIP_EXISTING" -eq 1 ]; then
    echo "$PROG: draft exists, --skip-existing set; leaving $DRAFT_PATH untouched" >&2
  else
    # YAML-escape the title/description for a double-quoted scalar: escape
    # backslash FIRST, then the double-quote (order matters). A double-quoted
    # YAML scalar treats both \ and " specially, so input like a regex (\d) or a
    # Windows path (C:\tmp) must have its backslashes doubled or octospec-lint
    # rejects the frontmatter as malformed YAML.
    y_title="${TITLE//\\/\\\\}"; y_title="${y_title//\"/\\\"}"
    y_desc="${DESCRIPTION//\\/\\\\}"; y_desc="${y_desc//\"/\\\"}"
    DRAFT_CONTENT="$(cat <<EOF
---
type: Rule
title: "$y_title"
description: "$y_desc"
tags: $TAGS_SEQ
timestamp: $TIMESTAMP
# --- octospec extension fields (OKF-permitted; consumers must preserve) ---
# DRAFT — generated by $PROG for slug "$SLUG". Scratch material for THIS PR.
# The author lands it into rules/<id>.md + rules/_index.yaml in this same PR;
# the PR review is the comprehension gate. The script never auto-writes rules/.
id: $RULE_ID
tier: $TIER
priority: $PRIORITY
load_bearing: $LOAD_BEARING
inject_when:
  paths: $PATHS_SEQ
  touches: $TOUCHES_SEQ
source: self
supersedes: []
status: draft
draft_source_slug: $SLUG
---

# $TITLE

$LEARNING

## Why load-bearing
<!-- Reviewer: state why every future task must obey this, or downgrade to an
     actor-scoped journal note (--kind=task) instead. -->
EOF
)"
    atomic_write "$DRAFT_PATH" "$DRAFT_CONTENT
"
    echo "$PROG: wrote rule draft -> $DRAFT_PATH" >&2
  fi

  # Promotion material -> stdout (unless suppressed).
  if [ "$NO_PROMOTE" -eq 1 ]; then
    echo "$PROG: --no-promote set; skipping promotion material" >&2
    exit 0
  fi

  REL_DRAFT=".octospec/learnings/pending/${SLUG}-rule-draft.md"
  REL_BRIEF=".octospec/tasks/${SLUG}/brief.md"
  cat <<EOF
## Rule reflow: $TITLE

**Linked task:** \`$REL_BRIEF\` (slug: \`$SLUG\`)
**Proposed rule:** id \`$RULE_ID\`, tier \`$TIER\`, priority \`$PRIORITY\`, load_bearing \`$LOAD_BEARING\`
**Draft (scratch material for this PR):** \`$REL_DRAFT\`

> Generated by \`$PROG\`. Use this material to land the rule IN THIS SAME PR:
> copy the draft into \`.octospec/rules/$RULE_ID.md\` and add the
> \`rules/_index.yaml\` entry. The PR review is the comprehension gate; the
> script does not auto-write rules/. Do NOT defer this to a separate PR.

### Proposed rule body
$LEARNING

### COMPREHENSION (answer to substance before merge)
1. **What does this rule actually constrain** on the load-bearing path? Describe
   the behavior every future task must follow, not the file list.
2. **What could break / be over-constrained** if this rule is injected widely?
   Name the dependents and the failure mode.
3. **How do we know it's right** — what evidence (the originating task, a repro,
   a trace) shows this should bind all future work rather than stay a heuristic?

### Author checklist (do these in this PR)
- [ ] inject_when (paths/touches) scopes the rule to where it actually applies.
- [ ] load_bearing/priority/tier are justified.
- [ ] Belongs in rules/ (binds everyone) vs journal/by-actor/ (one actor's note).
- [ ] Land the rule now: copy draft into \`.octospec/rules/$RULE_ID.md\`, add the
      \`rules/_index.yaml\` entry, log it in \`rules/log.md\`, drop the draft.
EOF
  exit 0
fi

# ============================================================================
# kind=task  -> per-actor journal entry, committed in-repo. No external memory.
# ============================================================================
if [ "$KIND" = "task" ]; then
  # Resolve the actor handle: explicit --actor, else git user.name, else "unknown".
  if [ -z "$ACTOR" ]; then
    ACTOR="$(git config user.name 2>/dev/null || true)"
  fi
  [ -n "$ACTOR" ] || ACTOR="unknown"
  ACTOR_ORIG="$ACTOR"
  # Normalize to the by-actor convention: lowercase, [a-z0-9-], starts with a letter.
  ACTOR="$(printf '%s' "$ACTOR" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-')"
  # collapse repeated dashes and trim leading/trailing dashes
  while case "$ACTOR" in *--*) true;; *) false;; esac; do ACTOR="${ACTOR//--/-}"; done
  ACTOR="${ACTOR#-}"
  ACTOR="${ACTOR%-}"
  # If normalization stripped every [a-z0-9] byte (CJK / punctuation / emoji
  # names — common on teams that use Chinese handles), ACTOR is now empty. A bare
  # "actor-" prefix would then collapse every such author into ONE shared lane and
  # silently last-writer-wins their journals. Derive a stable, per-name unique
  # handle from a hash of the ORIGINAL name instead, so 李雷 and 韩梅梅 land in
  # distinct lanes.
  if [ -z "$ACTOR" ]; then
    # Hash the original name. sha1sum is GNU-only (absent on a default macOS
    # PATH); fall back to BSD/macOS `shasum -a 1`. Without this fallback the
    # branch would yield an empty hash on macOS and collapse back into the bare
    # "actor-" collision lane this very code path exists to prevent.
    if command -v sha1sum >/dev/null 2>&1; then
      actor_hash="$(printf '%s' "$ACTOR_ORIG" | sha1sum | head -c 8)"
    else
      actor_hash="$(printf '%s' "$ACTOR_ORIG" | shasum -a 1 | head -c 8)"
    fi
    [ -n "$actor_hash" ] || die "could not hash actor name (no sha1sum/shasum?)"
    ACTOR="actor-$actor_hash"
  fi
  case "$ACTOR" in
    [a-z]*) :;;
    *) ACTOR="actor-$ACTOR";;   # ensure it starts with a letter
  esac
  # Re-trim in case the letter-prefix step left a trailing dash (e.g. "actor-").
  ACTOR="${ACTOR%-}"
  [ -n "$ACTOR" ] || die "could not derive a valid --actor handle"

  if [ -z "$TAGS" ]; then TAGS="octospec-learning,$SLUG"; fi
  TAGS_SEQ="$(csv_to_flow_seq "$TAGS")"
  if [ -z "$TITLE" ]; then TITLE="$(titlecase_slug "$SLUG")"; fi
  # YAML double-quoted scalar escape: backslash first, then double-quote.
  y_title="${TITLE//\\/\\\\}"; y_title="${y_title//\"/\\\"}"
  y_desc="${FIRST_LINE//\\/\\\\}"; y_desc="${y_desc//\"/\\\"}"

  JOURNAL_PATH="$BY_ACTOR_DIR/$ACTOR/${SLUG}.md"

  if [ -e "$JOURNAL_PATH" ] && [ "$SKIP_EXISTING" -eq 1 ]; then
    echo "$PROG: journal exists, --skip-existing set; leaving $JOURNAL_PATH untouched" >&2
    exit 0
  fi

  JOURNAL_CONTENT="$(cat <<EOF
---
type: Journal
title: "$y_title"
description: "$y_desc"
tags: $TAGS_SEQ
timestamp: $TIMESTAMP
# --- octospec extension fields ---
slug: $SLUG
actor: $ACTOR
source: self
---

# $TITLE

$LEARNING
EOF
)"
  atomic_write "$JOURNAL_PATH" "$JOURNAL_CONTENT
"
  echo "$PROG: wrote per-actor journal -> $JOURNAL_PATH" >&2
  # Echo the repo-relative path on stdout so a caller can chain (e.g. git add).
  printf '%s\n' ".octospec/journal/by-actor/$ACTOR/${SLUG}.md"
  exit 0
fi
