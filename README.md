# cachedoctor

**Find out why your LLM prompt cache isn't saving you money.**

Prompt caching cuts input cost up to ~90% — but it fails *silently*. A
misconfigured cache still returns a correct response; you just quietly pay full
price and never notice until the bill. cachedoctor tells you whether your cache
is working, *why* it isn't, and how much it's costing you.

## Install

```sh
go build -o cachedoctor .     # or: go install github.com/cachedoctor/cachedoctor@latest
```

## Use

```sh
cachedoctor observe                           # live proxy: diagnose real traffic (recommended)
cachedoctor check <request.json>              # scan one request for anti-patterns
cachedoctor diff  <callA.json> <callB.json>   # show what broke byte-identity
cachedoctor analyze <logs.jsonl>              # real hit rate + $/mo recoverable
```

`check` exits `2` if it finds a high-severity issue — drop it in CI.

**stdin:** any file argument can be `-`, so you can pipe instead of writing files:

```sh
cat req.json    | cachedoctor check -
curl ... | jq . | cachedoctor check -                 # straight from an API call
cat callB.json  | cachedoctor diff callA.json -        # one file, one piped
cat usage.jsonl | cachedoctor analyze -
```

## Observe live traffic (recommended — no code changes)

The easiest way to use cachedoctor: run it as a local pass-through and point your
app at it. It forwards every request **unchanged** (read-only — it never rewrites
or optimizes anything) and diagnoses what flows by.

```sh
cachedoctor observe                                 # listens on :7070
export ANTHROPIC_BASE_URL=http://localhost:7070     # (or OPENAI_BASE_URL=.../v1)
python your_app.py                                  # run your app exactly as usual
```

You get a live line per request — anti-patterns, prefix **drift** vs the previous
call, and a running hit rate — plus a session summary on Ctrl-C:

```
🔴 #1 anthropic /v1/messages → 200 · hit 0%  · No prompt caching enabled
🔴 #2 anthropic /v1/messages → 200 · hit 0%  · Cacheable prefix changed since the previous call
── session summary ──
2 requests · hit rate 0% · spent ~$0.42 · recoverable ~$18.30 (this session)
```

Works with any language (it's at the HTTP layer). It never stores your API key —
requests are forwarded verbatim. `--port N` and `--upstream URL` to customize.

## Feeding it files (CI / one-offs)

Prefer not to run a proxy? `check`/`diff`/`analyze` read the plain request and
response JSON your SDK already produces:

- **`check` / `diff`** — the request body you pass to the SDK. Capture it with
  `json.dump(req, open("req.json","w"))` (Python) or `JSON.stringify(req)` (TS),
  or pipe it in via `-`. For `diff`, capture two consecutive calls.
- **`analyze`** — a `.jsonl` of responses' `usage`. Already logging your API
  responses? Point `analyze` at those logs as-is — it reads Anthropic and OpenAI
  usage shapes.

## Use it in CI (GitHub Action)

Gate pull requests on cache/cost regressions. Save representative request bodies
as `*.cachedoctor.json`; the action runs `check` on them, `diff`s any that
changed against the PR's base branch, **fails the build** on a high-severity
regression, and comments the impact on the PR.

```yaml
# .github/workflows/cachedoctor.yml
name: cachedoctor
on: pull_request
jobs:
  cache-gate:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      pull-requests: write        # to comment on the PR
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0          # needed to diff against the base branch
      - uses: cachedoctor/cachedoctor@v1
        with:
          base-ref: origin/${{ github.base_ref }}
```

That turns "someone reordered the tools array and silently killed the cache" into
a red check on the PR instead of a surprise on next month's bill.

### `check` — catch the silent leak before you ship

```
$ cachedoctor check request.json
🔴 HIGH — Volatile content in the cached prefix
    Your tools/system prefix contains a volatile value (ISO timestamp:
    "2026-09-12T14:30"). If it changes between calls, the prefix is no longer
    byte-identical and every call misses — silently, at full price.
    fix: Move anything that changes (timestamps, IDs, dates) out of the cached
         prefix, or below the last cache_control breakpoint.
```

### `diff` — pinpoint what changed between two calls

```
$ cachedoctor diff callA.json callB.json
🔴 HIGH — Tools reordered between calls
    Same tools, different order:
      call A: search_code, run_tests
      call B: run_tests, search_code
    Reordering the tools array changes the cached bytes — the cache misses.
    fix: Emit tools in a stable, deterministic order every call.
```

Try it on the bundled [`examples/`](examples/).

## What it checks

**Anthropic (primary — manual caching has more ways to break):**

- No `cache_control` at all on a large, stable prefix
- Volatile content in the cached prefix (timestamps, dates, UUIDs, "today is…")
  — the #1 silent byte-identity breaker
- Default **5-minute TTL** (the 2026-03-06 regression) when reuse is spaced out
- Cacheable prefix below the model minimum (1024 / 2048 tokens) → silently ignored
- More than 4 cache breakpoints
- `diff`: tools reordered / changed, or system prompt drift (byte-level pinpoint)

**OpenAI (automatic caching):** OpenAI auto-caches the longest *byte-identical*
prefix ≥1024 tokens, so the manual rules (cache_control, TTL, breakpoints) don't
apply — but **prefix instability, prefix ordering, and minimum size do**, and
`check`/`diff`/`observe` check them:

- Volatile content in the prefix (a timestamp at the top breaks OpenAI's
  auto-cache exactly like Anthropic's — with no knob to debug it)
- Prompt below the 1024-token minimum (not cached at all)
- No stable leading system prefix (cacheable prefix starts at your variable turn)
- `diff` / `observe`: prefix drift between calls
- Streaming without `stream_options.include_usage` (OpenAI omits usage from the
  stream, so nothing downstream can see your hit rate). `observe` flags it; pass
  `--include-usage` to have the proxy inject it for you — the one deliberate
  exception to read-only.

Costs come from **[LiteLLM's model price table](https://github.com/BerriAI/litellm)**
(Anthropic + OpenAI), embedded in the binary; refresh with `scripts/update-pricing.sh`.

## Status

MVP: `observe`, `check`, `diff`, `analyze` — Anthropic + OpenAI, prices from LiteLLM.

## Contributing

Bug reports, ideas, and pull requests are welcome — please
[open an issue](https://github.com/cachedoctor/cachedoctor/issues).

## License

[Apache-2.0](LICENSE). Bundled pricing data is derived from LiteLLM (MIT) —
see [NOTICE](NOTICE).
