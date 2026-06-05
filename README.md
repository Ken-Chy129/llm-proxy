# CLI Proxy

Personal AI API proxy that exposes Claude (Vertex AI / OAuth) and OpenAI Codex (OAuth) as a unified OpenAI-compatible API.

## Features

- **OpenAI-compatible API** — `/v1/chat/completions`, `/v1/responses`, `/v1/images/generations`, `/v1/models`
- **Multiple backends** — Claude via Vertex AI, Claude via OAuth, Codex via OAuth
- **Multi-account pool** — Round-robin rotation across accounts with per-account quota tracking
- **Dynamic model discovery** — Auto-fetch available models from Codex backend on startup
- **Web dashboard** — Status overview, quota display, test chat, request logs, usage stats
- **SQLite logging** — Persistent request logs with aggregation queries
- **HTTPS** — Built-in TLS support
- **Login auth** — Username/password session for dashboard, Bearer token for API

## Supported Models

| Backend | Models | Auth |
|---------|--------|------|
| Vertex AI | claude-sonnet-4-6, claude-opus-4-6, claude-haiku-4-5 | GCP ADC |
| Claude OAuth | claude-sonnet-4-6, claude-opus-4-6 | Browser OAuth |
| Codex OAuth | gpt-5.5, gpt-5.4, gpt-5.4-mini, gpt-image-2 | Browser OAuth |

## Quick Start

```bash
# Build
go build -o cli-proxy .

# Configure
cp config.example.yaml config.yaml
# Edit config.yaml with your settings

# Run
./cli-proxy -config config.yaml
```

## Configuration

```yaml
server:
  port: 443
  api_key: "sk-your-api-key"          # Bearer token for API access
  cert_file: "/path/to/cert.pem"      # Optional: enable HTTPS
  key_file: "/path/to/key.pem"
  admin_user: "admin"                  # Dashboard login
  admin_password: "password"

vertex:
  project_id: "your-gcp-project-id"
  region: "us-east5"
  models:
    - name: "claude-sonnet-4-6"
      model: "claude-sonnet-4-6"
    - name: "claude-opus-4-6"
      model: "claude-opus-4-6"

claude_oauth:
  enabled: true
  models:
    - "claude-sonnet-4-6-oauth"
    - "claude-opus-4-6-oauth"

codex:
  enabled: true
  models:                              # Fallback; auto-fetched on login
    - "gpt-5.5"
    - "gpt-5.4"
    - "gpt-5.4-mini"
```

## API Usage

### Chat Completions

```bash
curl https://your-domain/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-api-key" \
  -d '{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}'
```

### Streaming

```bash
curl https://your-domain/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-api-key" \
  -d '{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":true}'
```

### Image Generation

```bash
curl https://your-domain/v1/images/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-api-key" \
  -d '{"model":"gpt-image-2","prompt":"A cat wearing sunglasses","size":"1024x1024"}'
```

### Responses API (Codex CLI)

```bash
export OPENAI_BASE_URL="https://your-domain/v1"
export OPENAI_API_KEY="sk-your-api-key"
codex
```

### Python SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://your-domain/v1",
    api_key="sk-your-api-key"
)

resp = client.chat.completions.create(
    model="claude-sonnet-4-6",
    messages=[{"role": "user", "content": "hello"}]
)
print(resp.choices[0].message.content)
```

## Dashboard

Visit `https://your-domain/` and login with admin credentials.

Features:
- Backend status with auth indicators
- Per-account quota display (plan type, rate limits, reset times)
- Model selector grouped by backend
- Test chat with streaming output
- Request logs with pagination
- Usage stats by model and day

## OAuth Account Management

### Adding accounts

1. Visit dashboard → click `+ Add Account` on Claude or Codex card
2. Complete OAuth login in browser
3. Token stored in `~/.cli-proxy/` and auto-refreshed

### Multi-account rotation

Requests are distributed across accounts using round-robin. Accounts with expired tokens are skipped. Tokens are auto-refreshed on startup.

## Deployment

### Cross-compile for Linux

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o cli-proxy-linux .
```

### Server setup

```bash
scp cli-proxy-linux root@server:~/cli-proxy/cli-proxy
scp config.yaml root@server:~/cli-proxy/
scp -r ~/.cli-proxy/*.json root@server:~/.cli-proxy/

ssh root@server
cd ~/cli-proxy && nohup ./cli-proxy -config config.yaml > /var/log/cli-proxy.log 2>&1 &
```

### Vertex AI on server

Upload GCP credentials:
```bash
scp ~/.config/gcloud/application_default_credentials.json root@server:~/.config/gcloud/
```

## Architecture

```
Client Request
     │
     ├─ /v1/chat/completions ─→ Router ─→ Executor ─→ Backend API
     ├─ /v1/responses        ─→ Codex passthrough ──→ chatgpt.com
     ├─ /v1/images/generations→ Codex tool call ────→ chatgpt.com
     └─ /v1/models           ─→ List all registered models
     
Executors:
  VertexExecutor     → OpenAI format ↔ Anthropic Messages API ↔ GCP Vertex AI
  ClaudeOAuthExecutor→ OpenAI format ↔ Anthropic Messages API ↔ api.anthropic.com
  CodexExecutor      → OpenAI format ↔ Codex Responses API   ↔ chatgpt.com
```

## Tech Stack

- Go + Gin
- SQLite (pure Go, modernc.org/sqlite)
- uTLS (Chrome fingerprint for Claude/Codex)
- Single binary, zero external dependencies

## License

MIT
