# glms

This project is written entirely by agents and is fully optimized for agent
vibe coding, debugging and RSI.

glms is a local API gateway in front of the GLM Coding Plan (z.ai). Tools talk
to `127.0.0.1:8787` with a throwaway local key; glms injects the real
coding-plan key, rewrites model names through an alias map, and fails over
across multiple keys when one hits a rate limit.

```
opencode / Claude Code / any tool
        │  Authorization: Bearer <listenKey>
        ▼
glms (127.0.0.1:8787)
        │  Authorization: Bearer <coding-plan key>, rotated on 401/403/429
        ▼
api.z.ai coding endpoints
```

## Endpoints

| Client request | Forwarded to |
| --- | --- |
| `POST /v1/chat/completions` | `https://api.z.ai/api/coding/paas/v4/chat/completions` |
| `POST /v1/responses` | `https://api.z.ai/api/v1/responses` |
| `POST /v1/messages` (+ `/v1/messages/count_tokens`) | `https://api.z.ai/api/anthropic/v1/messages` |
| `GET /v1/models` | local: the models configured below |
| `GET /health` | local |

Requests and responses pass through byte-for-byte (SSE streams included,
flushed per chunk); only the top-level `model` field is rewritten. The coding
endpoints are required: a coding-plan key is rejected by the general API
endpoint `https://api.z.ai/api/paas/v4`.

## Quick start

```sh
go build -o glms .
mkdir -p ~/.config/glms
cp config.example.json ~/.config/glms/config.json
# edit ~/.config/glms/config.json: put your coding-plan key in apiKeys
./glms
```

Verify with a live call:

```sh
curl -s http://127.0.0.1:8787/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}'
```

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

Restart opencode, run `/models` and pick a `glms` model. `apiKey: "glms"` is
arbitrary: it only has to match `listenKey` in the gateway config (empty
`listenKey` accepts anything, fine on localhost).

## Configuration

File: `~/.config/glms/config.json` (override with `-config`; fields left out
keep the defaults, the `models` map is merged with the defaults).

| Field | Default | Meaning |
| --- | --- | --- |
| `listen` | `127.0.0.1:8787` | Listen address; localhost only by default |
| `listenKey` | `""` (open) | Key tools must send (`Authorization: Bearer` or `X-Api-Key`) |
| `apiKeys` | — | Coding-plan keys, tried round-robin; rotate on 401/403/429 |
| `upstreams.openaiChat` | z.ai coding endpoint | Base URL appended with `/chat/completions` |
| `upstreams.openaiResponses` | z.ai responses endpoint | Base URL appended with `/responses` |
| `upstreams.anthropic` | z.ai anthropic endpoint | Base URL appended with `/v1/messages` |
| `models` | `glm-5.3`, `glm-5.3-flash`, `glm-5.2` | Alias → upstream model; unknown names pass through unchanged |

`GLMS_API_KEY` (comma-separated) replaces `apiKeys`, so keys can stay out of
files. `-listen` overrides `listen`.

## Behavior notes

- One log line per proxied request on stdout: path, model (alias→target if
  rewritten), key index used, status, bytes in/out, duration. Key rotations
  are logged with the upstream error body (first 200 chars, key material
  never).
- Failover replays the buffered request with the next key only while nothing
  has been sent to the client yet — always true, because the response is
  forwarded only after the loop picks the final response. Network errors and
  other statuses are passed through as-is.
- Gateway-generated errors use the shape of the protocol on that route
  (Anthropic `type: "error"` on `/v1/messages`, OpenAI `error` elsewhere).
