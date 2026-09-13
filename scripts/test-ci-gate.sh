#!/usr/bin/env bash
# Scenario tests for ci-gate.sh — the gate must be FAIL-CLOSED: every way it
# can't do its job must fail the build, never pass it. Needs bash >= 4.4
# (same as the gate itself) and a go toolchain. Self-contained: builds the
# binary and works in a throwaway git repo.
#
#   ./scripts/test-ci-gate.sh
set -euo pipefail
cd "$(dirname "$0")/.."
REPO_DIR=$(pwd)
GATE="$REPO_DIR/scripts/ci-gate.sh"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
go build -o "$WORK/cachedoctor" .

# a throwaway git repo with fixtures, so base-ref scenarios are hermetic
mkdir -p "$WORK/repo/nested/deep" "$WORK/repo/with space"
cd "$WORK/repo"
git init -q -b main
git config user.email t@t && git config user.name t
cp "$REPO_DIR/examples/good.json"      good.cachedoctor.json
cp "$REPO_DIR/examples/no-cache.json"  "with space/high.cachedoctor.json"
cp "$REPO_DIR/examples/good.json"      nested/deep/n.cachedoctor.json
git add -A && git commit -qm base

pass=0; fail=0
expect() { # expect <rc> <name> [env overrides...]
  local want="$1" name="$2"; shift 2
  local rc=0
  env CACHEDOCTOR_BIN="$WORK/cachedoctor" "$@" bash "$GATE" >/dev/null 2>&1 || rc=$?
  if [ "$rc" = "$want" ]; then pass=$((pass+1));
  else fail=$((fail+1)); echo "FAIL: $name (rc=$rc want=$want)"; fi
}

expect 0 "clean fixture"                 CACHEDOCTOR_FIXTURES=good.cachedoctor.json
expect 1 "HIGH fixture (space in path)"  CACHEDOCTOR_FIXTURES="with space/high.cachedoctor.json"
expect 1 "HIGH via globstar (default glob, nested dirs)"
expect 0 "no fixtures matched"           CACHEDOCTOR_FIXTURES='zzz-*.json'
expect 1 "missing binary"                CACHEDOCTOR_BIN=/definitely/not/here CACHEDOCTOR_FIXTURES=good.cachedoctor.json
expect 1 "binary is a directory"         CACHEDOCTOR_BIN="$WORK" CACHEDOCTOR_FIXTURES=good.cachedoctor.json
expect 1 "unresolvable base ref"         CACHEDOCTOR_FIXTURES=good.cachedoctor.json CACHEDOCTOR_BASE=no-such-ref
expect 0 "valid base ref, unchanged"     CACHEDOCTOR_FIXTURES=good.cachedoctor.json CACHEDOCTOR_BASE=HEAD

echo 'not json {{{' > broken.cachedoctor.json
expect 1 "unparseable fixture"           CACHEDOCTOR_FIXTURES=broken.cachedoctor.json
rm broken.cachedoctor.json

# regression detection: mutate a clean fixture vs the base commit
python3 - <<'PY'
import json
d = json.load(open('good.cachedoctor.json'))
s = d.get('system')
if isinstance(s, list): s[0]['text'] += ' DRIFTED'
else: d['system'] = (s or '') + ' DRIFTED'
json.dump(d, open('good.cachedoctor.json', 'w'))
PY
out=$(env CACHEDOCTOR_BIN="$WORK/cachedoctor" CACHEDOCTOR_FIXTURES=good.cachedoctor.json \
      CACHEDOCTOR_BASE=HEAD bash "$GATE" 2>/dev/null || true)
if grep -q 'regression vs' <<<"$out"; then pass=$((pass+1));
else fail=$((fail+1)); echo "FAIL: regression section for a changed fixture"; fi
git checkout -q good.cachedoctor.json

echo "ci-gate scenarios: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
