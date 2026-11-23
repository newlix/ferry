# ferry

This project is written entirely by agents and is fully optimized for agent
vibe coding, debugging and RSI.

ferry is a local API gateway in front of the GLM Coding Plan (z.ai). You add a
GLM Coding Plan key once; after that ferry picks the right key for every
request (quota left, plan expiry), keeps each conversation on one account
(prompt-cache friendly), and fails over across keys on rate limits. Tools
talk to `127.0.0.1:8787` and never see a real key.

```
opencode / Claude Code / any tool
        │
        ▼
ferry (127.0.0.1:8787)
        │  Authorization: Bearer <coding-plan key>
        │  key order: smooth weighted round-robin; keys about to lose unused weekly quota gain weight
        │  one conversation → one key (sticky); 401/403/429 → next key + rebind
        │  (`route` in config.json pins every request to one key instead)
        ▼
api.z.ai coding endpoints
```

## Quick start

```sh
go build -o /usr/local/bin/ferry .
ferry            # first run: paste your GLM Coding Plan key (input hidden), name it, then it serves
ferry status     # keys, plans, quota, running state
ferry route      # show routing; ferry route NAME fixes it to one key, ferry route auto unfixes
ferry install    # start at login (logs: ~/.config/ferry/ferry.log)
```

Keys are verified when added (plan name, quota, expiry are fetched and
shown) and stored in the macOS Keychain; on other systems they fall back to
`~/.config/ferry/keys.json` (0600). `ferry key add` and `ferry key remove NAME`
manage them (`ferry status` lists them). After a key or routing change the
running ferry picks it up at once: launchd restarts the service, and
everywhere else `ferry` SIGHUPs the running process, which re-reads the
config file and key store without dropping a request. By default ferry is
quiet — start it with `-v` to log every request.

## opencode

`~/.config/opencode/opencode.json`:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "ferry": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "GLM Coding Plan (ferry)",
      "options": {
        "baseURL": "http://127.0.0.1:8787/v1",
        "apiKey": "ferry"
      },
      "models": {
        "glm-5.3": { "name": "GLM-5.3" },
        "glm-5.3-flash": { "name": "GLM-5.3 Flash" },
        "glm-5.2": { "name": "GLM-5.2" }
      }
    }
  },
  "model": "ferry/glm-5.3"
}
```

Restart opencode, run `/models` and pick a `ferry` model. The `apiKey` value
is a placeholder the gateway ignores; localhost is the security boundary.

## Endpoints

| Client request | Forwarded to |
| --- | --- |
| `POST /v1/chat/completions` | `https://api.z.ai/api/coding/paas/v4/chat/completions` |
| `POST /v1/responses` | `https://api.z.ai/api/v1/responses` |
| `POST /v1/messages` (+ `/v1/messages/count_tokens`) | `https://api.z.ai/api/anthropic/v1/messages` |
| `GET /v1/models` | local: the models configured below |
| `GET /v1/quota` | local: the home page's data as JSON (per key: plan, plan end, weekly and 5-hour `left` 0..1 with `resetsAt`; `null` windows while unknown) |
| `GET /` | local: a page with each key's weekly and 5-hour quota left, plan end date, and the base URL to use |
| `GET /health` | local |

Requests and responses pass through byte-for-byte (SSE streams included,
flushed per chunk); only the top-level `model` field is rewritten. The coding
endpoints are required: a coding-plan key is rejected by the general API
endpoint `https://api.z.ai/api/paas/v4`. BigModel (bigmodel.cn) plans are not
supported yet.

## Configuration

Optional file `~/.config/ferry/config.json` — everything has a default:

| Field | Default | Meaning |
| --- | --- | --- |
| `listen` | `127.0.0.1:8787` | Listen address; localhost only |
| `models` | `glm-5.3`, `glm-5.3-flash`, `glm-5.2` | Alias → upstream model (merged with the defaults); unknown names pass through unchanged |
| `retryWait` | `"5m"` (0 disables) | How long a request is held and retried when EVERY key answers 401/403/429. z.ai's request rate limit (code 1302) is a short-window throttle that clears in seconds to minutes, and agent clients kill a whole run on one 429 — holding turns a dead run into a pause. When the deadline passes the last failover response is forwarded unchanged. Accepts a Go duration string or seconds. |
| `route` | unset (automatic) | Set with `ferry route NAME` (unfix with `ferry route auto`): every request uses that key and never rotates to another — a 401/403/429 holds and retries it per `retryWait`. A name that is not in the key store fails at startup. Unset: automatic routing — weighted round-robin, sticky conversations, failover across keys. |

The one config most tools need is an alias, so a client-baked model name
reaches a real model:

```jsonc
{"models": {"claude-sonnet-4-5": "glm-5.3"}}
```

Keys live in the Keychain, never in this file; a config that still carries
`apiKeys` or `upstreams` from an older ferry fails loudly with a hint.

## Behavior notes

- Key selection is smooth weighted round-robin. Every 5 minutes (and right
  after any 429 failover) ferry reads each key's remaining 5-hour and weekly
  quota, reset times and plan name from the console endpoints z.ai's own
  subscription UI uses (`/api/monitor/usage/quota/limit` and
  `/api/biz/subscription/list`, undocumented but stable in practice).
  Normally every usable key has equal weight, so traffic spreads evenly.
  When a key's weekly window resets within 48 hours its weight ramps up
  linearly, up to +3× at the reset and scaled by the remaining weekly quota:
  unused weekly credits do not roll over, so they are burned down before the
  reset — gradually, so the other keys keep their share and no account's
  rate limits become the bottleneck. Keys whose quota reads empty are tried
  last; a passed reset time refills the window instead of demoting it.
- Selection is sticky per conversation on top of that. The protocols carry
  no session identifier, so a conversation is identified by a hash of the
  model plus the first message of the request (the system prompt for coding
  agents — the only element guaranteed identical across turns); every request
  with that prefix uses the key the conversation started with, until that
  key runs out of quota (skipped pre-emptively) or returns 401/403/429
  (rebound to the key that worked). On multi-account setups this keeps
  prompt-cache hits inside one account. Bindings expire after 30 idle
  minutes. A batch client whose independent requests share one system
  prompt sends `X-Ferry-Sticky: off`: those requests go round-robin instead
  of piling onto one key's rate limit.
- Quiet output is the product; `-v` adds one line per request (path, model
  alias→target, key name, conversation hash, status, bytes, duration). Key
  rotations, quota fetch failures and held requests always print. Key
  material never appears in logs.
- When every key answers a failover status in one sweep, the request is
  held and the sweep retried with backoff until `retryWait` (5m default).
  Live incident that shaped this (2026-11-18): one key weekly-exhausted
  (1310), the other tripping the short-window request limit (1302) under
  parallel agent load — a retry seconds later succeeds, so ferry waits
  instead of handing the 429 to a client that would abort its whole run.
- Gateway-generated errors use the shape of the protocol on that route
  (Anthropic `type: "error"` on `/v1/messages`, OpenAI `error` elsewhere).
