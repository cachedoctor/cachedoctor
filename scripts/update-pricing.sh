#!/usr/bin/env sh
# Regenerate pricing.json from LiteLLM's model price table (Anthropic + OpenAI).
set -e
cd "$(dirname "$0")/.."  # pricing.json lives at the repo root
URL=https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json
curl -sL "$URL" -o /tmp/litellm_prices.json
python3 - <<'PY'
import json
d = json.load(open('/tmp/litellm_prices.json'))
out = {}
for k, v in d.items():
    if not isinstance(v, dict): continue
    prov = v.get('litellm_provider')
    if prov not in ('anthropic', 'openai'): continue
    inp = v.get('input_cost_per_token')
    if not inp: continue
    read = v.get('cache_read_input_token_cost')
    write = v.get('cache_creation_input_token_cost')
    if read is None:  read = inp * (0.1 if prov == 'anthropic' else 0.5)
    if write is None: write = inp * (1.25 if prov == 'anthropic' else 1.0)
    out[k.lower()] = [round(inp*1e6, 4), round(read*1e6, 4), round(write*1e6, 4)]
json.dump(out, open('pricing.json', 'w'), separators=(',', ':'), sort_keys=True)
print(f"pricing.json: {len(out)} Anthropic/OpenAI models")
PY

echo "reminder: review familyRep in pricing.go — representatives should point at each family's newest priced entry."
