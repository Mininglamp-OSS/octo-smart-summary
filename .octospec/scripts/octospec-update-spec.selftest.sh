#!/usr/bin/env bash
# Self-test for octospec-update-spec.sh — exercises all three reflow calls and
# asserts the products. Runs against a throwaway slug namespace (selftest-*) and
# cleans up the drafts it creates, so it never leaves the repo dirty.
# Exit 0 = all pass.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/octospec-update-spec.sh"
# Run against an isolated throwaway .octospec/ fixture so the test never depends
# on (or pollutes) the directory the script ships in. The script honors the
# exported OCTOSPEC_DIR; the fixture's parent acts as the repo root.
FIXTURE_ROOT="$(mktemp -d)"
export OCTOSPEC_DIR="$FIXTURE_ROOT/.octospec"
mkdir -p "$OCTOSPEC_DIR/learnings/pending" "$OCTOSPEC_DIR/journal/by-actor" "$OCTOSPEC_DIR/rules"
PENDING="$OCTOSPEC_DIR/learnings/pending"
BY_ACTOR="$OCTOSPEC_DIR/journal/by-actor"
TEST_ACTOR="selftest-bot"

pass=0
fail=0

# check <description> -- runs the rest of the args as a command; pass iff it
# exits 0. Keeps assertions free of `A && B || C` foot-guns (no SC2015).
check() {
  local desc="$1"; shift
  if "$@"; then
    echo "  ok: $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL: $desc" >&2
    fail=$((fail + 1))
  fi
}
contains() { printf '%s' "$2" | grep -qF -- "$1"; }
# refuses: succeeds iff the script exits non-zero for the given args.
refuses()  { ! "$SCRIPT" "$@" >/dev/null 2>&1; }
# refuses_stdin: like refuses but feeds empty stdin (for the no-learning case).
refuses_stdin() { ! "$SCRIPT" "$@" </dev/null >/dev/null 2>&1; }

cleanup() {
  rm -f "$PENDING"/selftest-*-rule-draft.md
  rm -rf "${BY_ACTOR:?}/$TEST_ACTOR"
  # default-actor test writes selftest-defactor.md under the git user's lane.
  find "$BY_ACTOR" -name 'selftest-defactor.md' -delete 2>/dev/null || true
}
# Single EXIT trap: scrub the test artifacts AND drop the throwaway fixture root,
# so repeated local runs never leak temp dirs.
on_exit() { cleanup; rm -rf "$FIXTURE_ROOT"; }
trap on_exit EXIT
cleanup

echo "== 1. --kind=rule (draft + promotion material) =="
BODY="$("$SCRIPT" --slug selftest-rule --kind rule --load-bearing \
  --inject-touches "space,audit" --priority 88 \
  --learning $'Cross-Space writes must record an audit entry.\nUse audit.Record before returning.')"
DRAFT="$PENDING/selftest-rule-rule-draft.md"
check "draft file created" test -f "$DRAFT"
check "OKF type: Rule" grep -qx 'type: Rule' "$DRAFT"
for f in title description tags timestamp id tier priority load_bearing inject_when source; do
  check "frontmatter has $f" grep -qE "^(  )?${f}:" "$DRAFT"
done
check "--load-bearing -> true" grep -q 'load_bearing: true' "$DRAFT"
check "--priority honored" grep -q 'priority: 88' "$DRAFT"
check "inject touches present" grep -q '"audit"' "$DRAFT"
check "marked draft" grep -q 'status: draft' "$DRAFT"
check "promotion block has COMPREHENSION" contains 'COMPREHENSION' "$BODY"
nq="$(printf '%s\n' "$BODY" | grep -cE '^[0-9]+\. \*\*')"
check "promotion block has >=3 questions ($nq)" test "$nq" -ge 3
check "promotion block Linked task" contains 'Linked task' "$BODY"
check "promotion block links draft" contains 'selftest-rule-rule-draft.md' "$BODY"

echo "== 2. idempotency (rerun must not duplicate) =="
"$SCRIPT" --slug selftest-rule --kind rule --learning 'second run overwrites' --no-promote >/dev/null
n_after="$(find "$PENDING" -maxdepth 1 -name 'selftest-rule-rule-draft.md' | wc -l | tr -d ' ')"
check "still exactly one draft after rerun" test "$n_after" -eq 1
check "default overwrites content" grep -q 'second run overwrites' "$DRAFT"
"$SCRIPT" --slug selftest-rule --kind rule --learning 'THIRD run skipped' --skip-existing --no-promote >/dev/null
check "--skip-existing preserves prior draft" grep -q 'second run overwrites' "$DRAFT"

echo "== 3. --kind=task (per-actor journal entry, committed in-repo) =="
OUT="$("$SCRIPT" --slug selftest-task --kind task --actor "$TEST_ACTOR" \
  --learning $'Reviewers should grep for raw c.JSON in error paths.\nIt is a recurring i18n bypass.' \
  --tags "review-pattern,i18n")"
JOURNAL="$BY_ACTOR/$TEST_ACTOR/selftest-task.md"
check "journal entry created" test -f "$JOURNAL"
check "task kind wrote NOTHING to learnings/pending" test ! -e "$PENDING/selftest-task-rule-draft.md"
check "stdout echoes repo-relative path" contains "journal/by-actor/$TEST_ACTOR/selftest-task.md" "$OUT"
check "OKF type: Journal" grep -qx 'type: Journal' "$JOURNAL"
for f in title description tags timestamp slug actor source; do
  check "frontmatter has $f" grep -qE "^${f}:" "$JOURNAL"
done
check "slug recorded" grep -q '^slug: selftest-task$' "$JOURNAL"
check "actor recorded" grep -q "^actor: $TEST_ACTOR\$" "$JOURNAL"
check "learning body present" grep -q 'recurring i18n bypass' "$JOURNAL"
check "tags carried (review-pattern)" grep -q '"review-pattern"' "$JOURNAL"
check "no external/memory leakage in output" sh -c "! printf '%s' \"\$1\" | grep -qiE 'nowledge|nmem|payload|space:'" _ "$OUT"

echo "== 4. --kind=task default actor + idempotency =="
"$SCRIPT" --slug selftest-task --kind task --actor "$TEST_ACTOR" \
  --learning 'second run overwrites journal' >/dev/null
n_journals="$(find "$BY_ACTOR/$TEST_ACTOR" -maxdepth 1 -name 'selftest-task.md' | wc -l | tr -d ' ')"
check "still exactly one journal after rerun" test "$n_journals" -eq 1
check "default overwrites journal content" grep -q 'second run overwrites journal' "$JOURNAL"
"$SCRIPT" --slug selftest-task --kind task --actor "$TEST_ACTOR" \
  --learning 'THIRD run skipped' --skip-existing >/dev/null 2>&1
check "--skip-existing preserves prior journal" grep -q 'second run overwrites journal' "$JOURNAL"
# default actor derives from git user.name and is normalized to [a-z0-9-]
DEF2="$("$SCRIPT" --slug selftest-defactor --kind task --learning 'x')"
check "default actor path is normalized + emitted" contains 'journal/by-actor/' "$DEF2"
REPO_ROOT="$(cd "$OCTOSPEC_DIR/.." && pwd)"
DEF_PATH="$REPO_ROOT/$(printf '%s' "$DEF2" | sed -n '1p')"
check "default actor file actually written" test -f "$DEF_PATH"
check "default actor handle is [a-z0-9-]" sh -c "printf '%s' \"\$1\" | grep -qE '^\\.octospec/journal/by-actor/[a-z][a-z0-9-]*/selftest-defactor\\.md$'" _ "$DEF2"
rm -f "$DEF_PATH"; rmdir "$(dirname "$DEF_PATH")" 2>/dev/null || true

echo "== 5. input validation / refusals =="
check "missing --kind refused" refuses --slug ok-slug --learning x
check "bad --kind refused" refuses --slug ok-slug --kind bogus --learning x
check "bad slug refused" refuses --slug Bad_Slug --kind task --learning x
check "empty learning refused" refuses_stdin --slug ok-slug --kind task
# A value-taking option must not silently swallow the NEXT option as its value.
check "flag-as-value (--title --priority) refused" \
  refuses --title --priority 88 --slug ok-slug --kind task --learning x
check "flag-as-value (--learning --kind) refused" \
  refuses --slug ok-slug --kind task --learning --kind
# ...but the --opt=val escape hatch still accepts a value that starts with --.
check "--opt=--value escape hatch accepted" \
  "$SCRIPT" --slug ok-slug --kind task --actor t --title=--weird --learning x

echo "== 6. backslash in learning -> valid YAML frontmatter (octospec-lint) =="
# A learning whose first line carries a regex (\d) and a Windows path (C:\tmp):
# the default description copies that line into a double-quoted YAML scalar, so a
# backslash that is not doubled produces malformed YAML and fails octospec-lint.
BS_LEARNING=$'Match \\d+ digits and the path C:\\tmp\\report before returning.\nKeep the backslashes intact.'

# Locate the linter if it ships alongside the consuming repo's scripts; else fall
# back to a self-contained YAML parse (the exact check octospec-lint performs).
LINT=""
for cand in "$HERE/octospec-lint.py" "$HERE/../../../scripts/octospec-lint.py"; do
  [ -f "$cand" ] && { LINT="$cand"; break; }
done

# yaml_ok <file>: succeeds iff the file's --- frontmatter --- block parses as YAML.
yaml_ok() {
  python3 - "$1" <<'PY'
import sys, yaml
text = open(sys.argv[1], encoding="utf-8").read().replace("\r\n", "\n")
lines = text.split("\n")
if not lines or lines[0].strip() != "---":
    sys.exit("no frontmatter")
close = next((i for i in range(1, len(lines)) if lines[i].strip() == "---"), None)
if close is None:
    sys.exit("unterminated frontmatter")
data = yaml.safe_load("\n".join(lines[1:close]))
sys.exit(0 if isinstance(data, dict) and data.get("type") else "bad frontmatter")
PY
}

# rule path: draft frontmatter must be valid YAML despite the backslashes.
"$SCRIPT" --slug selftest-bs --kind rule --no-promote \
  --title 'Regex \d guard' --learning "$BS_LEARNING" >/dev/null
BS_DRAFT="$PENDING/selftest-bs-rule-draft.md"
check "backslash rule draft created" test -f "$BS_DRAFT"
check "backslash rule draft is valid YAML frontmatter" yaml_ok "$BS_DRAFT"
check "backslash preserved in draft body" grep -qF 'C:\tmp\report' "$BS_DRAFT"

# task path: journal lives under journal/by-actor/ which IS in octospec-lint scope.
"$SCRIPT" --slug selftest-bs --kind task --actor "$TEST_ACTOR" \
  --learning "$BS_LEARNING" >/dev/null
BS_JOURNAL="$BY_ACTOR/$TEST_ACTOR/selftest-bs.md"
check "backslash journal created" test -f "$BS_JOURNAL"
check "backslash journal is valid YAML frontmatter" yaml_ok "$BS_JOURNAL"
check "backslash preserved in journal body" grep -qF 'C:\tmp\report' "$BS_JOURNAL"
if [ -n "$LINT" ]; then
  # Lint from the fixture root so the relative path keeps its journal/by-actor/
  # prefix (octospec-lint's knowledge-scope is path-based); the backslash journal
  # is in scope and must pass.
  check "octospec-lint passes the backslash journal" \
    python3 "$LINT" "$OCTOSPEC_DIR"
else
  echo "  note: octospec-lint.py not found alongside scripts; used self-contained YAML parse"
fi
rm -f "$BS_DRAFT" "$BS_JOURNAL"

echo "== 7. CJK actor names must not collide into one lane =="
# tr -c 'a-z0-9-' '-' maps every byte of a pure-CJK / punctuation name to '-',
# which after collapse+trim leaves an empty handle. The fix derives a per-name
# hash so distinct authors get distinct lanes instead of silently overwriting
# each other under a shared 'actor-' lane.
OUT_A="$("$SCRIPT" --slug selftest-cjk --kind task --actor '李雷' --learning 'lei learning')"
OUT_B="$("$SCRIPT" --slug selftest-cjk --kind task --actor '韩梅梅' --learning 'mei learning')"
LANE_A="$(printf '%s' "$OUT_A" | sed -n '1p')"
LANE_B="$(printf '%s' "$OUT_B" | sed -n '1p')"
check "CJK actor A wrote a journal" test -f "$OCTOSPEC_DIR/../$LANE_A"
check "CJK actor B wrote a journal" test -f "$OCTOSPEC_DIR/../$LANE_B"
check "CJK actors land in DISTINCT lanes (no last-writer-wins)" test "$LANE_A" != "$LANE_B"
check "CJK lane A is not the bare 'actor-' lane" sh -c "case \"\$1\" in */by-actor/actor-/*) exit 1;; *) exit 0;; esac" _ "$LANE_A"
check "CJK lane handle is [a-z][a-z0-9-]*" sh -c "printf '%s' \"\$1\" | grep -qE '/by-actor/[a-z][a-z0-9-]*/selftest-cjk\\.md$'" _ "$LANE_A"
check "李雷's learning is intact in its own lane" grep -q 'lei learning' "$OCTOSPEC_DIR/../$LANE_A"
check "韩梅梅's learning is intact in its own lane" grep -q 'mei learning' "$OCTOSPEC_DIR/../$LANE_B"
# same CJK name -> same lane (stable, not random)
OUT_A2="$("$SCRIPT" --slug selftest-cjk2 --kind task --actor '李雷' --learning 'again')"
LANE_A2="$(printf '%s' "$OUT_A2" | sed -n '1p')"
check "same CJK name maps to a stable lane" \
  test "$(dirname "$LANE_A")" = "$(dirname "$LANE_A2")"

echo "== 8. literal newline in --title/--description folds to single-line YAML =="
# --description is consumed by the rule path (the task path derives description
# from the learning's first line), so exercise the rule path to cover both.
"$SCRIPT" --slug selftest-nl --kind rule --no-promote \
  --title "$(printf 'line one\nline two')" \
  --description "$(printf 'desc a\ndesc b')" --learning 'body' >/dev/null
NL_DRAFT="$PENDING/selftest-nl-rule-draft.md"
check "newline-title draft created" test -f "$NL_DRAFT"
check "newline-title draft is valid YAML frontmatter" yaml_ok "$NL_DRAFT"
check "title folded to one line" grep -q '^title: "line one line two"$' "$NL_DRAFT"
check "description folded to one line" grep -q '^description: "desc a desc b"$' "$NL_DRAFT"

# Scrub the stray journals these sections created (outside the selftest-* / actor
# fixtures that cleanup() handles).
find "$BY_ACTOR" -name 'selftest-cjk.md'  -delete 2>/dev/null || true
find "$BY_ACTOR" -name 'selftest-cjk2.md' -delete 2>/dev/null || true
rm -f "$NL_DRAFT"
# section 5's --opt=--value escape-hatch check wrote actor 't' / slug 'ok-slug'.
find "$BY_ACTOR" -name 'ok-slug.md' -delete 2>/dev/null || true

echo "== 9. actor hash fallback works without GNU sha1sum (macOS/BSD) =="
# The empty-normalization branch hashes the actor name. sha1sum is GNU-only; on a
# default macOS PATH only `shasum` exists. Run the script under a stub PATH that
# exposes shasum but NOT sha1sum and assert a CJK actor still gets a non-empty,
# unique lane (on Linux the ambient GNU sha1sum would otherwise mask this break).
STUB_BIN="$FIXTURE_ROOT/stub-bin"
mkdir -p "$STUB_BIN"
for b in shasum git tr head awk sed dirname basename mkdir mv cat date rm grep \
         env bash sh printf find sort comm wc; do
  p="$(command -v "$b" 2>/dev/null)" && ln -sf "$p" "$STUB_BIN/$b"
done
check "stub PATH really hides sha1sum" \
  sh -c "! PATH=\"\$1\" command -v sha1sum >/dev/null 2>&1" _ "$STUB_BIN"
NOSHA_OUT="$(PATH="$STUB_BIN" "$SCRIPT" --slug selftest-nosha --kind task \
  --actor '李雷' --learning 'no-sha1sum lei' 2>/dev/null)"
NOSHA_LANE="$(printf '%s' "$NOSHA_OUT" | sed -n '1p')"
check "CJK actor gets a journal even without sha1sum" \
  test -f "$OCTOSPEC_DIR/../$NOSHA_LANE"
check "fallback lane is a non-empty actor-XXXXXXXX (not bare 'actor-')" \
  sh -c "printf '%s' \"\$1\" | grep -qE '/by-actor/actor-[0-9a-f]{8}/selftest-nosha\\.md$'" _ "$NOSHA_LANE"
find "$BY_ACTOR" -name 'selftest-nosha.md' -delete 2>/dev/null || true

echo "== 10. --rule-id must be kebab-case (no YAML-breaking chars) =="
check "rule-id with ':' refused" refuses --slug ok-slug --kind rule --rule-id 'bad:id' --learning x
check "rule-id with '/' refused"  refuses --slug ok-slug --kind rule --rule-id 'a/b' --learning x
check "rule-id starting non-letter refused" refuses --slug ok-slug --kind rule --rule-id '1abc' --learning x
# a clean kebab-case rule-id is accepted and lands as the id: scalar.
"$SCRIPT" --slug selftest-rid --kind rule --no-promote --rule-id 'custom-rule-id' \
  --learning 'rid body' >/dev/null
RID_DRAFT="$PENDING/selftest-rid-rule-draft.md"
check "valid --rule-id accepted" test -f "$RID_DRAFT"
check "valid --rule-id written as id: scalar" grep -q '^id: custom-rule-id$' "$RID_DRAFT"
rm -f "$RID_DRAFT"

echo
echo "RESULT: $pass passed, $fail failed"
test "$fail" -eq 0
