#!/usr/bin/env bash
# cachedoctor CI gate — run `check` on request fixtures (and `diff` changed ones
# against the base branch), summarize, comment on the PR, and fail the build on
# any high-severity cache/cost regression.
#
# Fail-closed by design: a missing binary, an unparseable fixture, or a bad
# base ref FAILS the gate — a gate that can't run must never read as green.
#
# Config (env):
#   CACHEDOCTOR_FIXTURES  glob of request JSON fixtures  (default **/*.cachedoctor.json)
#   CACHEDOCTOR_BASE      base git ref to diff against   (optional; enables regression detection)
#   CACHEDOCTOR_BIN       cachedoctor binary             (default: cachedoctor on PATH)
#   CACHEDOCTOR_PR        PR number for the comment      (optional)
#   GITHUB_TOKEN          token for `gh pr comment`      (optional)
set -uo pipefail

if ((BASH_VERSINFO[0] < 4)); then
  echo "cachedoctor: this gate needs bash >= 4 (for globstar); found $BASH_VERSION" >&2
  exit 1
fi

FIXTURES="${CACHEDOCTOR_FIXTURES:-**/*.cachedoctor.json}"
BASE_REF="${CACHEDOCTOR_BASE:-}"
BIN="${CACHEDOCTOR_BIN:-cachedoctor}"

command -v "$BIN" >/dev/null 2>&1 || { [ -f "$BIN" ] && [ -x "$BIN" ]; } || {
  echo "cachedoctor: binary '$BIN' not found — refusing to pass a gate that can't run" >&2
  exit 1
}

if [ -n "$BASE_REF" ]; then
  git rev-parse --verify --quiet "$BASE_REF^{commit}" >/dev/null || {
    echo "cachedoctor: base ref '$BASE_REF' does not resolve — did the checkout fetch it (fetch-depth)?" >&2
    exit 1
  }
fi

shopt -s globstar nullglob
# compgen splits on newlines, not IFS — paths with spaces survive.
files=()
while IFS= read -r f; do files+=("$f"); done < <(compgen -G "$FIXTURES" || true)
if [ ${#files[@]} -eq 0 ]; then
  echo "cachedoctor: no request fixtures matched '$FIXTURES' — nothing to check."
  exit 0
fi

# Fixtures that exist in the base ref and differ from it (--diff-filter=M),
# resolved in one git call instead of per-file probes.
changed=""
if [ -n "$BASE_REF" ]; then
  changed="$(git -c core.quotepath=off diff --name-only --diff-filter=M "$BASE_REF" -- "${files[@]}" 2>/dev/null)"
fi

fail=0
errored=0
body="## 🩺 cachedoctor — prompt-cache / cost gate"$'\n'
for f in "${files[@]}"; do
  out="$("$BIN" check "$f" 2>&1)"; rc=$?
  case $rc in
    0) ;;
    2) fail=1 ;;
    *) errored=1; out="⚠️ check failed (exit $rc) — a fixture the gate can't check fails the gate:"$'\n'"$out" ;;
  esac
  # 4-backtick fences: fixture-derived text containing ``` can't break out.
  body+=$'\n'"<details><summary><code>$f</code></summary>"$'\n\n'
  body+='````'$'\n'"$out"$'\n''````'$'\n'"</details>"$'\n'

  # regression check: diff a changed fixture against its base-branch version
  if [ -n "$changed" ] && printf '%s\n' "$changed" | grep -qxF "$f"; then
    out="$(git show "$BASE_REF:$f" | "$BIN" diff - "$f" 2>&1)"; rc=$?
    case $rc in
      0) ;;
      2) fail=1 ;;
      *) errored=1; out="⚠️ diff failed (exit $rc):"$'\n'"$out" ;;
    esac
    body+="**regression vs \`$BASE_REF\`:**"$'\n\n''````'$'\n'"$out"$'\n''````'$'\n'
  fi
done

status="✅ no cache/cost regressions found"
if [ "$fail" -eq 1 ] && [ "$errored" -eq 1 ]; then
  status="🔴 **cache/cost regression detected** · ⚠️ some fixtures could not be checked — see details"
elif [ "$fail" -eq 1 ]; then
  status="🔴 **cache/cost regression detected** — see details below"
elif [ "$errored" -eq 1 ]; then
  status="⚠️ **gate error** — some fixtures could not be checked (see details)"
fi
body="$status"$'\n'"$body"

# Always write the run summary; post a PR comment when we have the context.
[ -n "${GITHUB_STEP_SUMMARY:-}" ] && printf '%s\n' "$body" >> "$GITHUB_STEP_SUMMARY"
printf '%s\n' "$body"
if [ -n "${GITHUB_TOKEN:-}" ] && [ -n "${CACHEDOCTOR_PR:-}" ]; then
  printf '%s\n' "$body" | \
    gh pr comment "$CACHEDOCTOR_PR" --edit-last --create-if-none --body-file - \
    || echo "cachedoctor: PR comment not posted (non-fatal)" >&2
fi

if [ "$fail" -eq 1 ]; then
  echo "cachedoctor: failing the build — high-severity cache/cost finding(s)."
  exit 1
fi
if [ "$errored" -eq 1 ]; then
  echo "cachedoctor: failing the build — fixture(s) could not be checked."
  exit 1
fi
echo "cachedoctor: clean."
