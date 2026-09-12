#!/usr/bin/env bash
# cachedoctor CI gate — run `check` on request fixtures (and `diff` changed ones
# against the base branch), summarize, comment on the PR, and fail the build on
# any high-severity cache/cost regression.
#
# Config (env):
#   CACHEDOCTOR_FIXTURES  glob of request JSON fixtures  (default **/*.cachedoctor.json)
#   CACHEDOCTOR_BASE      base git ref to diff against   (optional; enables regression detection)
#   CACHEDOCTOR_BIN       cachedoctor binary             (default: cachedoctor on PATH)
#   CACHEDOCTOR_PR        PR number for the comment      (optional)
#   GITHUB_TOKEN          token for `gh pr comment`      (optional)
set -uo pipefail

FIXTURES="${CACHEDOCTOR_FIXTURES:-**/*.cachedoctor.json}"
BASE_REF="${CACHEDOCTOR_BASE:-}"
BIN="${CACHEDOCTOR_BIN:-cachedoctor}"

shopt -s globstar nullglob
files=( $FIXTURES )
if [ ${#files[@]} -eq 0 ]; then
  echo "cachedoctor: no request fixtures matched '$FIXTURES' — nothing to check."
  exit 0
fi

# Fixtures that exist in the base ref and differ from it (--diff-filter=M),
# resolved in one git call instead of per-file probes.
changed=""
if [ -n "$BASE_REF" ]; then
  changed="$(git diff --name-only --diff-filter=M "$BASE_REF" -- "${files[@]}" 2>/dev/null)"
fi

fail=0
body="## 🩺 cachedoctor — prompt-cache / cost gate"$'\n'
for f in "${files[@]}"; do
  out="$("$BIN" check "$f" 2>&1)"; rc=$?
  [ "$rc" -eq 2 ] && fail=1
  body+=$'\n'"<details><summary><code>$f</code></summary>"$'\n\n'
  body+='```'$'\n'"$out"$'\n''```'$'\n'"</details>"$'\n'

  # regression check: diff a changed fixture against its base-branch version
  if [ -n "$changed" ] && printf '%s\n' "$changed" | grep -qxF "$f"; then
    out="$(git show "$BASE_REF:$f" | "$BIN" diff - "$f" 2>&1)"; rc=$?
    [ "$rc" -eq 2 ] && fail=1
    body+="**regression vs \`$BASE_REF\`:**"$'\n\n''```'$'\n'"$out"$'\n''```'$'\n'
  fi
done

status="✅ no cache/cost regressions found"
[ "$fail" -eq 1 ] && status="🔴 **cache/cost regression detected** — see details below"
body="$status"$'\n'"$body"

# Always write the run summary; post a PR comment when we have the context.
[ -n "${GITHUB_STEP_SUMMARY:-}" ] && printf '%s\n' "$body" >> "$GITHUB_STEP_SUMMARY"
printf '%s\n' "$body"
if [ -n "${GITHUB_TOKEN:-}" ] && [ -n "${CACHEDOCTOR_PR:-}" ]; then
  printf '%s\n' "$body" | \
    gh pr comment "$CACHEDOCTOR_PR" --edit-last --create-if-none --body-file - 2>/dev/null || true
fi

if [ "$fail" -eq 1 ]; then
  echo "cachedoctor: failing the build — high-severity cache/cost finding(s)."
  exit 1
fi
echo "cachedoctor: clean."
