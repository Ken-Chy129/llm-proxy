# LLM Proxy

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?logo=docker&logoColor=white)](#docker)

[简体中文](README.md) | **English**

Lightweight AI API proxy built for **Claude Code** and **Codex CLI**. Unifies Claude (Vertex AI / OAuth), OpenAI Codex (OAuth), and Kimi API behind compatible API endpoints with multi-account pooling, 429 failover, multi-key management, usage analytics, and a built-in dashboard.

![Dashboard — Stats](docs/dashboard-stats.png)

## Features

- **Multi-protocol** — OpenAI `/v1/chat/completions`, `/v1/responses`, `/v1/images/generations` + Anthropic `/v1/messages` passthrough or translation
- **Drop-in compatible** — Works directly with Claude Code, Codex CLI, and OpenAI SDKs
- **Multi-backend routing** — Vertex AI, Claude OAuth, Codex OAuth, Kimi API — auto-dispatched by model name
- **Account pooling + failover** — Round-robin load balancing; on an upstream 429 the request fails over to the next account, and expired tokens are auto-skipped
- **Multi API-key management** — Issue per-caller keys with individual daily token limits; create/revoke from the dashboard
- **Visual analytics** — Time-series trend plus breakdowns by model / key / backend / account (timezone-aware)
- **Live config** — Edit each backend's model list and admin credentials from the dashboard; model changes apply instantly
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
  models:
    - name: "kimi-k3"
      model: "kimi-k3"
```

Claude Code:

```bash
export ANTHROPIC_BASE_URL="https://your-domain"
export ANTHROPIC_AUTH_TOKEN="sk-your-proxy-key"
export ANTHROPIC_MODEL="kimi-k3"
claude
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

| Backend | Models | Auth |
|---------|--------|------|
| Vertex AI | claude-sonnet-4-6, claude-opus-4-6, claude-haiku-4-5 | GCP credentials (ADC / dashboard upload) |
| Claude OAuth | claude-sonnet-4-6-oauth, claude-opus-4-6-oauth, claude-opus-4-8-oauth | Browser OAuth |
| Codex OAuth | gpt-5.5, gpt-5.4, gpt-5.4-mini, gpt-image-2 | Browser OAuth |
| Kimi API | kimi-k3, kimi-k2.7-code-highspeed, kimi-k2.6 | `MOONSHOT_API_KEY` environment variable |

> Model lists are editable from the dashboard's **Config** tab; Codex auto-fetches its available models after login.

## Auth & API Keys

Callers authenticate with `Authorization: Bearer <key>`, from one of two sources:

- **Multi API-key (recommended)** — Issue a separate key per caller on the dashboard's **Keys** tab, each with its own daily token limit, usage view, and revoke control.
- **Single key (optional)** — Set one global key via `server.api_key` in `config.yaml` (leave empty to disable auth for trusted networks).

## Configuration

```yaml
server:
  port: 9090
  # api_key: "sk-proxy-xxx"            # Optional global key; the Keys tab is usually more flexible
  admin_user: "admin"                  # Dashboard login
  admin_password: "password"
  cert_file: "/path/to/cert.pem"       # Optional: enable HTTPS
  key_file: "/path/to/key.pem"

vertex:
  project_id: "your-gcp-project-id"
  region: "us-east5"
  models:
    - name: "claude-sonnet-4-6"       # Name exposed to clients
      model: "claude-sonnet-4-6"      # Actual Vertex AI model ID

claude_oauth:
  enabled: true
  token_dir: "/data"                   # Token & DB storage (defaults to ~/.llm-proxy; use /data for Docker)
  models:
    - "claude-sonnet-4-6-oauth"
    - "claude-opus-4-6-oauth"
    - "claude-opus-4-8-oauth"

codex:
  enabled: true
  models:                              # Fallback list; auto-fetched after login
    - "gpt-5.5"
    - "gpt-5.4"

kimi:
  enabled: true
  base_url: "https://api.moonshot.cn/v1"
  api_key_env: "MOONSHOT_API_KEY"
  models:
    - name: "kimi-k3"
      model: "kimi-k3"
```

## Dashboard

Visit `http://your-domain:9090/` and login with admin credentials.

**Backends** — Backend status, account pools, quota details. Account indicators are operational: 🟢 usable / 🔴 rate-limited upstream / ⚪ paused (an expired OAuth access token auto-refreshes, so it isn't flagged).

![Dashboard — Backends](docs/dashboard-backends.png)

**Stats** — Time-series trend (toggle requests / tokens, timezone-aware), with a breakdown by model / key / backend / account below.

Other tabs: **Chat** (streaming test chat), **Image** (image generation), **Logs** (paginated request logs), **Keys** (API keys & daily limits), **Config** (edit model lists and admin credentials live).

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
  ├─ /v1/chat/completions   → Router → Executor ────────→ Backend API
  ├─ /v1/responses          → Passthrough/translate ────→ chatgpt.com / Kimi
  ├─ /v1/images/generations → Codex tool call ──────────→ chatgpt.com
  └─ /v1/models             → List all registered models

Executors:
  VertexExecutor       → OpenAI ↔ Anthropic Messages API ↔ GCP Vertex AI
  ClaudeOAuthExecutor  → OpenAI ↔ Anthropic Messages API ↔ api.anthropic.com
  CodexExecutor        → OpenAI ↔ Codex Responses API    ↔ chatgpt.com
  KimiExecutor         → OpenAI/Responses/Anthropic      ↔ Kimi Chat Completions
```

## Tech Stack

- **Go** + Gin
- **SQLite** — pure Go (modernc.org/sqlite)
- **uTLS** — Chrome TLS fingerprint for Claude/Codex requests
- **Docker** — multi-stage build, ~15MB image

## License

[MIT](LICENSE)
