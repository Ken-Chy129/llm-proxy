# LLM Proxy

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?logo=docker&logoColor=white)](#docker)

[简体中文](README.md) | **English**

Lightweight AI API proxy built for **Claude Code** and **Codex CLI**. Unifies Claude (Vertex AI / OAuth / Anthropic-compatible relays), OpenAI Codex (OAuth), Kimi API, and AnyGen API behind compatible API endpoints with multi-account pooling, 429 failover, multi-key management, usage analytics, and a built-in dashboard.

![Dashboard — Stats](docs/dashboard-stats.png)

## Features

- **Multi-protocol** — OpenAI `/v1/chat/completions`, `/v1/responses`, `/v1/images/generations` + Anthropic `/v1/messages` passthrough or translation
- **Drop-in compatible** — Works directly with Claude Code, Codex CLI, and OpenAI SDKs
- **Multi-backend routing** — Vertex AI, Claude OAuth, Anthropic-compatible relays, Codex OAuth, Kimi API, AnyGen API — auto-dispatched by model name
- **Account pooling + failover** — Round-robin load balancing; on an upstream 429 the request fails over to the next account, and expired tokens are auto-skipped
- **Multi API-key management** — Issue per-caller keys with individual daily token limits; create/revoke from the dashboard
- **Visual analytics** — Time-series trend plus breakdowns by model / key / backend / account (timezone-aware)
- **Live config** — Edit backend model lists, routing priority, and admin credentials from the dashboard; changes apply instantly
- **Single binary** — Pure Go (including SQLite), no CGO, cross-compile and deploy
- **Docker ready** — One command to start

## Quick Start

### Docker

```bash
# 1. Configure
cp config.example.yaml config.yaml
# Edit config.yaml, set token_dir: "/data"

# 2. Start
docker compose up -d

# 3. Open dashboard
open http://localhost:9090
```

### Build from source

```bash
go build -o llm-proxy .
cp config.example.yaml config.yaml
./llm-proxy -config config.yaml
```

## Integration

### Claude Code

```bash
export ANTHROPIC_BASE_URL="https://your-domain"
export ANTHROPIC_API_KEY="sk-your-api-key"
claude
```

Requests pass through natively to Vertex AI / Claude OAuth — thinking blocks, prompt caching, and tool use all work.

### Codex CLI

Add to `~/.codex/config.toml`:

```toml
model_provider = "llm-proxy"
model = "gpt-5.5"

[model_providers.llm-proxy]
name = "LLM Proxy"
base_url = "https://your-domain/v1"
env_key = "LLM_PROXY_API_KEY"
wire_api = "responses"
```

Or use environment variables:

```bash
export OPENAI_BASE_URL="https://your-domain/v1"
export OPENAI_API_KEY="sk-your-api-key"
codex
```

### Kimi through the proxy

The Kimi key is read only from an environment variable and is never persisted in `config.yaml`:

```bash
export MOONSHOT_API_KEY="your-new-kimi-api-key"
```

```yaml
kimi:
  enabled: true
  base_url: "https://api.moonshot.cn/v1"
  api_key_env: "MOONSHOT_API_KEY"
  api_format: "openai"
```

Then publish the model against it:

```yaml
models:
  - name: "kimi-k3"
    providers: [kimi]
```

Keys created in the **Kimi Code console** are separate from API Platform keys. Use the Kimi Code Anthropic-compatible endpoint instead:

```yaml
kimi:
  enabled: true
  base_url: "https://api.kimi.com/coding"
  api_key_env: "MOONSHOT_API_KEY"
  api_format: "anthropic"

models:
  - name: "kimi-k3"
    providers:
      - kimi: "k3"                 # upstream only knows this id
  - name: "kimi-for-coding"
    providers: [kimi]
  - name: "kimi-for-coding-highspeed"
    providers: [kimi]
```

Kimi Coding may accept an unknown model string without rejecting the request, so the upstream IDs must exactly match `/v1/models`. K3's upstream ID is `k3`, not `kimi-k3`.

### Anthropic-compatible relay

The relay token is read only from the environment and is never persisted:

```bash
export ANTHROPIC_AUTH_TOKEN="your-relay-token"
```

```yaml
relay:
  enabled: true
  base_url: "http://34.80.212.77/api"
  auth_token_env: "ANTHROPIC_AUTH_TOKEN"

models:
  - name: "claude-opus-5"
    providers: [claude_oauth, relay]   # overflow to the relay when the subscription is out
  - name: "claude-fable-5"
    providers: [relay]
```

Claude Code `/v1/messages` traffic is passed through natively; OpenAI Chat Completions and Responses traffic is translated by the proxy. All five models above were verified end-to-end. This relay's `/v1/models` response is incomplete: `claude-opus-5` and `claude-fable-5` work when called directly even though they are not advertised.

### AnyGen through the proxy

The AnyGen `sk-ag` key is read only from an environment variable:

```bash
export ANYGEN_LLM_KEY="your_sk-ag_key"
```

Grant the key exactly the app's `Chat Completions` and `Models` actions with `whole_app=false`, then configure the app-scoped API base URL:

```yaml
anygen:
  enabled: true
  base_url: "https://www.anygen.io/v1/openapi/anyclaw/app/appg4oo4fl2ay7g2u7my4eaqzy/api/v1"
  api_key_env: "ANYGEN_LLM_KEY"

models:
  - name: "gpt-5.6-luna"
    providers: [anygen]
```

At startup the proxy calls upstream `GET /models` and registers the visible models dynamically. The model-list request invokes no model and consumes no credits. AnyGen Chat Completions is non-streaming only; `stream:true` is rejected explicitly. Credits are fetched from the platform-native `GET https://www.anygen.io/v1/openapi/key/verify` endpoint, outside the app's `/api/v1` base URL, and shown on the AnyGen Backend card.

Claude Code:

```bash
export ANTHROPIC_BASE_URL="https://your-domain"
export ANTHROPIC_AUTH_TOKEN="sk-your-proxy-key"
# Add K3 to /model without replacing Opus, Sonnet, or Haiku
export ANTHROPIC_CUSTOM_MODEL_OPTION="kimi-k3"
export ANTHROPIC_CUSTOM_MODEL_OPTION_NAME="Kimi K3 via LLM Proxy"
export ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION="Kimi K3 coding model"
claude
```

These environment variables affect only the current shell and its child
processes; they do not modify `~/.claude/settings.json`. Do not map
`ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_OPUS_MODEL`,
`ANTHROPIC_DEFAULT_SONNET_MODEL`, or `ANTHROPIC_DEFAULT_HAIKU_MODEL` to
`kimi-k3`, because that makes Claude Code display K3 as the default and for all
three built-in model slots.

For a one-off K3 session without adding it to `/model`:

```bash
ANTHROPIC_BASE_URL="https://your-domain" \
ANTHROPIC_AUTH_TOKEN="sk-your-proxy-key" \
claude --model kimi-k3
```

Codex CLI uses the same custom provider configuration shown above, with `model = "kimi-k3"` and `wire_api = "responses"`. Anthropic Messages and Responses requests are translated to Kimi Chat Completions. Text streaming and tool calls are supported; Anthropic-only features such as thinking signatures and context management are not preserved completely.

### OpenAI SDK

```python
from openai import OpenAI

client = OpenAI(base_url="https://your-domain/v1", api_key="sk-your-api-key")
resp = client.chat.completions.create(
    model="claude-sonnet-4-6",
    messages=[{"role": "user", "content": "hello"}]
)
```

### API

```bash
# Chat
curl https://your-domain/v1/chat/completions \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}],"stream":true}'

# Image generation
curl https://your-domain/v1/images/generations \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-image-2","prompt":"A cat wearing sunglasses","size":"1024x1024"}'
```

## Supported Models

There is one published catalogue, defined by the `models` section of `config.yaml`. A model can name several providers, and the proxy decides which one answers — callers always see `claude-opus-5` and never need to know whether the subscription, Vertex, or a relay served it.

| Provider | Serves | Auth |
|---------|--------|------|
| Claude OAuth | Claude models | Browser OAuth |
| Codex OAuth | GPT models, gpt-image-2 | Browser OAuth |
| Vertex AI | Claude models | GCP credentials (ADC / dashboard upload) |
| Kimi Code | Kimi models | `MOONSHOT_API_KEY` environment variable |
| Relay | Claude models via an Anthropic-compatible upstream | `ANTHROPIC_AUTH_TOKEN` environment variable |
| AnyGen | Whatever its `/models` endpoint advertises (GPT, Gemini, Claude, DeepSeek, …) | `ANYGEN_LLM_KEY` environment variable |

> Models and their provider order are editable from the dashboard's **Config** tab and take effect on save.

## Auth & API Keys

Callers authenticate with `Authorization: Bearer <key>`, from one of two sources:

- **Multi API-key (recommended)** — Issue a separate key per caller on the dashboard's **Keys** tab, each with its own daily token limit, usage view, and revoke control.
- **Single key (optional)** — Set one global key via `server.api_key` in `config.yaml` (leave empty to disable auth for trusted networks).

## Configuration

```yaml
server:
  port: 9090
  # api_key: "sk-proxy-xxx"            # Optional global key; the Keys tab is usually more flexible
  admin_user: "admin"                  # Dashboard login; empty refuses every login
  admin_password: "password"            # Fallback: LLM_PROXY_ADMIN_USER / LLM_PROXY_ADMIN_PASSWORD
  tray_token: "tray-..."               # Read-only credential for the desktop widget (/api/tray); unset locks it out
  cert_file: "/path/to/cert.pem"       # Optional: enable HTTPS
  key_file: "/path/to/key.pem"

# A provider section is connection detail only — it no longer decides what it serves
claude_oauth:
  enabled: true
  token_dir: "/data"                   # Token & DB storage (defaults to ~/.llm-proxy; use /data for Docker)

codex:
  enabled: true

vertex:
  enabled: true
  project_id: "your-gcp-project-id"
  region: "us-east5"

kimi:
  enabled: true
  base_url: "https://api.moonshot.cn/v1"
  api_key_env: "MOONSHOT_API_KEY"
  api_format: "openai"

relay:
  enabled: true
  base_url: "http://34.80.212.77/api"
  auth_token_env: "ANTHROPIC_AUTH_TOKEN"

anygen:
  enabled: true
  base_url: "https://www.anygen.io/v1/openapi/anyclaw/app/appg4oo4fl2ay7g2u7my4eaqzy/api/v1"
  api_key_env: "ANYGEN_LLM_KEY"

# Default order per series. A model's series comes from its name prefix.
series:
  claude: [claude_oauth, vertex, relay]
  gpt: [codex, anygen]
  gemini: [anygen]
  kimi: [kimi]

# The published catalogue. `providers` is the failover chain, tried top to bottom.
models:
  - name: "claude-opus-5"              # No providers → inherits series.claude
  - name: "claude-haiku-4-5"
    providers:
      - vertex: "claude-haiku-4-5-20251001"   # Some upstreams only know dated ids
      - relay: "claude-haiku-4-5-20251001"
  - name: "gpt-5.5"
  - name: "kimi-k3"
    providers:
      - kimi: "k3"
```

### How routing is decided

A model's `providers` list is both its priority order and its failover chain: the first provider that is **enabled, credentialed, and not rate-limited** serves the request, and the next one takes over when it can't. So `claude-opus-5: [claude_oauth, relay]` means "use the subscription, and when it runs out, pay the relay" — worth deciding deliberately, because the spend is automatic.

The `{vertex: claude-haiku-4-5-20251001}` form is the one rename that remains, and it is purely a connection detail: clients, pricing, and stats all see `claude-haiku-4-5`.

## Dashboard

Visit `http://your-domain:9090/` and login with admin credentials.

**Backends** — Backend status, account pools, quota details. Account indicators are operational: 🟢 usable / 🔴 rate-limited upstream / ⚪ paused (an expired OAuth access token auto-refreshes, so it isn't flagged).

![Dashboard — Backends](docs/dashboard-backends.png)

**Stats** — Time-series trend (toggle requests / tokens / cost / errors, timezone-aware), with a breakdown by model / key / backend / account below.

**Config → Models** — the published model is the subject, grouped by series. Collapsed, a model is one line: name, provider chain, who is serving it, and its rate. Expanded, you can reorder the chain, append a fallback, and set an upstream rename (the `↦` slot stays ghosted until you hover it or it holds a value). The **use** button on a provider row is a *temporary* switch: it pins the model to that provider for 30 minutes at runtime only, never touching `config.yaml`, and expires back to the configured order — trying a provider out should not leave a permanent trace. **Series Defaults** below is the starting chain for newly added models; editing it does not rewrite models that already have one. Models, Series and Admin save independently, and an unsaved edit lights its panel's Save button.

**Cost accounting** — every request is priced as it is recorded, from a built-in table of published list rates (Anthropic / OpenAI / Moonshot), with input, cache read, cache write and output billed separately. The figure is *list API price*: Claude Code and Codex subscription traffic is not billed per request, so it answers "what would these tokens have cost on the pay-per-token API".

Rates are a published property of each model rather than a deployment setting, so there is nothing to configure and nothing to edit on the dashboard: one model, one rate, regardless of which provider served it. A model with no price anywhere is reported as *unknown* rather than $0 — otherwise it would look free — and shows an amber `unpriced` on its row. The startup log and `GET /api/pricing` list them. Existing history is priced once, on the first start after upgrading.

Other tabs: **Chat** (streaming test chat), **Image** (image generation), **Logs** (paginated request logs), **Keys** (API keys & daily limits), **Config** (edit model routing and admin credentials live).

### Account Management

1. Click **+ Add Account** on a Backends card
2. Complete OAuth login in the browser
3. Tokens are saved and auto-refreshed on startup

Requests are distributed via round-robin; on an upstream 429 a request fails over to the next account, and expired tokens are skipped automatically.

## Deployment

### Docker

```bash
docker compose up -d
```

`docker-compose.yaml` mounts `config.yaml` read-only; data (tokens, SQLite) is persisted in a Docker volume.

For Vertex AI, uncomment the GCP credentials mount in `docker-compose.yaml`:

```yaml
volumes:
  - ./gcp-credentials.json:/data/gcp-credentials.json:ro
environment:
  - GOOGLE_APPLICATION_CREDENTIALS=/data/gcp-credentials.json
```

### Binary

```bash
# Cross-compile
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o llm-proxy-linux .

# Deploy
scp llm-proxy-linux root@server:~/llm-proxy/llm-proxy
scp config.yaml root@server:~/llm-proxy/
nohup ./llm-proxy -config config.yaml > /var/log/llm-proxy.log 2>&1 &
```

## Architecture

```
Client Request
  │
  ├─ /v1/messages           → Router → Passthrough/translate → Claude / Vertex / Kimi
  ├─ /v1/chat/completions   → Router → Executor ────────→ Backend API (AnyGen: non-streaming only)
  ├─ /v1/responses          → Passthrough/translate ────→ chatgpt.com / Kimi
  ├─ /v1/images/generations → Codex tool call ──────────→ chatgpt.com
  └─ /v1/models             → List all registered models

Executors:
  VertexExecutor       → OpenAI ↔ Anthropic Messages API ↔ GCP Vertex AI
  ClaudeOAuthExecutor  → OpenAI ↔ Anthropic Messages API ↔ api.anthropic.com
  CodexExecutor        → OpenAI ↔ Codex Responses API    ↔ chatgpt.com
  KimiExecutor         → OpenAI/Responses/Anthropic      ↔ Kimi Chat Completions
  AnyGenExecutor       → OpenAI Chat Completions         ↔ AnyGen App API
```

## Tech Stack

- **Go** + Gin
- **SQLite** — pure Go (modernc.org/sqlite)
- **uTLS** — Chrome TLS fingerprint for Claude/Codex requests
- **Docker** — multi-stage build, ~15MB image

## License

[MIT](LICENSE)
