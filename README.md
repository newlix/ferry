# glms

This project is written entirely by agents and is fully optimized for agent
vibe coding, debugging and RSI.

glms is a local API gateway in front of the GLM Coding Plan (z.ai). You add
your coding-plan key once; after that glms picks the right key for every
request (quota left, plan expiry), keeps each conversation on one account
(prompt-cache friendly), and fails over across keys on rate limits. Tools
talk to `127.0.0.1:8787` and never see a real key.

```
opencode / Claude Code / any tool
        │
        ▼
glms (127.0.0.1:8787)
        │  Authorization: Bearer <coding-plan key>
        │  key order: smooth weighted round-robin; near-expiry keys gain weight (≤48h)
        │  one conversation → one key (sticky); 401/403/429 → next key + rebind
        ▼
api.z.ai coding endpoints
```

## Quick start

```sh
go build -o /usr/local/bin/glms .
glms            # first run: paste your coding-plan key (hidden input), then it serves
glms status     # plans, quota bars, service state
glms install    # run at login as a LaunchAgent (logs: ~/.config/glms/glms.log)
```

Keys are verified when added (plan name, quota, expiry are fetched and
shown) and stored in the macOS Keychain; on other systems they fall back to
`~/.config/glms/keys.json` (0600). `glms key add`, `glms key remove NAME`,
`glms key list` manage them; the running service restarts itself after a
key change. By default glms is quiet — start it with `-v` to log every
request.

## opencode

`~/.config/opencode/opencode.json`:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "glms": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "GLM Coding Plan (glms)",
      "options": {
        "baseURL": "http://127.0.0.1:8787/v1",
        "apiKey": "glms"
      },
      "models": {
        "glm-5.3": { "name": "GLM-5.3" },
        "glm-5.3-flash": { "name": "GLM-5.3 Flash" },
        "glm-5.2": { "name": "GLM-5.2" }
      }
    }
  },
  "model": "glms/glm-5.3"
}
```

Restart opencode, run `/models` and pick a `glms` model. The `apiKey` value
is a placeholder the gateway ignores; localhost is the security boundary.

## Endpoints

| Client request | Forwarded to |
| --- | --- |
| `POST /v1/chat/completions` | `https://api.z.ai/api/coding/paas/v4/chat/completions` |
| `POST /v1/responses` | `https://api.z.ai/api/v1/responses` |
| `POST /v1/messages` (+ `/v1/messages/count_tokens`) | `https://api.z.ai/api/anthropic/v1/messages` |
| `GET /v1/models` | local: the models configured below |
| `GET /v1/usage` | local: per-key quota, plan and expiry (`glms status` is the human view) |
| `GET /health` | local |

Requests and responses pass through byte-for-byte (SSE streams included,
flushed per chunk); only the top-level `model` field is rewritten. The coding
endpoints are required: a coding-plan key is rejected by the general API
endpoint `https://api.z.ai/api/paas/v4`. BigModel (bigmodel.cn) plans are not
supported yet.

## Configuration

Optional file `~/.config/glms/config.json` — everything has a default:

| Field | Default | Meaning |
| --- | --- | --- |
| `listen` | `127.0.0.1:8787` | Listen address; localhost only |
| `models` | `glm-5.3`, `glm-5.3-flash`, `glm-5.2` | Alias → upstream model (merged with the defaults); unknown names pass through unchanged |

Keys live in the Keychain, never in this file; a config that still carries
`apiKeys` or `upstreams` from an older glms fails loudly with a hint.

## Behavior notes

- Key selection is smooth weighted round-robin. Every 5 minutes (and right
  after any 429 failover) glms reads each key's remaining 5-hour and weekly
  quota, plan name and renewal date from the console endpoints z.ai's own
  subscription UI uses (`/api/monitor/usage/quota/limit` and
  `/api/biz/subscription/list`, undocumented but stable in practice).
  Normally every usable key has equal weight, so traffic spreads evenly.
  Within the last 48 hours before a plan's expiry its weight ramps up
  linearly, up to +3× at expiry and scaled by its remaining weekly quota:
  traffic shifts toward it gradually instead of all at once, and the other
  keys keep their share, so one account's rate limits never become the
  bottleneck. Keys whose quota reads empty, or whose expiry has passed, are
  tried last.
- Selection is sticky per conversation on top of that. The protocols carry
  no session identifier, so a conversation is identified by a hash of the
  model plus the first message of the request (the system prompt for coding
  agents — the only element guaranteed identical across turns); every request
  with that prefix uses the key the conversation started with, until that
  key runs out of quota (skipped pre-emptively) or returns 401/403/429
  (rebound to the key that worked). On multi-account setups this keeps
  prompt-cache hits inside one account. Bindings expire after 30 idle
  minutes.
- Quiet output is the product; `-v` adds one line per request (path, model
  alias→target, key index, conversation hash, status, bytes, duration). Key
  rotations and quota fetch failures always print. Key material never
  appears in logs.
- Gateway-generated errors use the shape of the protocol on that route
  (Anthropic `type: "error"` on `/v1/messages`, OpenAI `error` elsewhere).
