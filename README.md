# Claude API Gateway

Enterprise-grade internal API gateway for managing and load-balancing Claude API requests across multiple upstream accounts.

## Architecture

```
Employee App ──► Gateway (:8080/v1/messages) ──► Anthropic API
                    │
                    ├── Auth (employee API key validation)
                    ├── Rate Limit (per-key daily limits)
                    ├── Pool (least-loaded account selection)
                    ├── Session Affinity (multi-turn stickiness)
                    ├── Silent Failover (transparent retry on 429/5xx)
                    ├── SOCKS5 Proxy (optional per-account proxy binding)
                    └── Async Logging (non-blocking batch writes)
```

## Features

| Feature | Description |
|---------|-------------|
| **Account Pool** | Multiple upstream accounts with least-loaded scheduling |
| **Session Affinity** | Multi-turn conversations stick to the same account via `session-id` header |
| **Silent Failover** | Automatic retry with a different account on 429/5xx (configurable attempts) |
| **Per-Key Rate Limiting** | Daily request limits per employee API key |
| **Per-Account RPM/Concurrency** | Configurable RPM and max concurrent requests per account |
| **SOCKS5 Proxy** | Optional proxy binding per account (auto-assigned from pool) |
| **Async Logging** | Non-blocking batch writes to SQLite (configurable buffer/flush) |
| **Token Expiry Monitoring** | Automatic isolation of accounts with expiring tokens |
| **Admin Dashboard** | Modern web UI with real-time monitoring, charts, and CRUD |
| **Health Check** | `/health` endpoint for monitoring/load balancers |
| **Log Purge** | Automatic cleanup of logs older than 90 days |

## Quick Start

### Local (requires Go 1.22+)

```bash
cd claude-gateway
go mod tidy
go build -o gateway .
./gateway
```

Open http://localhost:8080 in your browser.

### Docker

```bash
# Auto-generated admin key
docker compose up -d

# Custom admin key
ADMIN_KEY=your-secret docker compose up -d
```

### Docker (manual)

```bash
docker build -t claude-gateway .
docker run -d -p 8080:8080 \
  -v gateway_data:/data \
  -e ADMIN_KEY=your-secret \
  claude-gateway
```

## Configuration

All settings via environment variables. See `.env.example` for full list.

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | Server listen port |
| `DB_PATH` | `gateway.db` | SQLite database file path |
| `ADMIN_KEY` | auto-generated | Admin API/UI authentication key |
| `LOG_LEVEL` | `info` | Log level: debug, info, warn, error |
| `UPSTREAM_URL` | `https://api.anthropic.com` | Upstream API base URL |
| `DEFAULT_RPM` | `60` | Default requests/minute per account |
| `DEFAULT_MAX_CONCUR` | `5` | Default max concurrent requests per account |
| `MAX_RETRY_ATTEMPTS` | `3` | Max retry attempts with different accounts |
| `POOL_RELOAD_INTERVAL` | `2m` | How often to reload accounts from DB |
| `TOKEN_REFRESH_LEAD` | `30m` | Mark accounts as refreshing this long before expiry |
| `LOG_CHANNEL_SIZE` | `8192` | Async log buffer size |
| `LOG_FLUSH_SIZE` | `200` | Batch size for log writes |
| `LOG_FLUSH_INTERVAL` | `2s` | Max interval between log flushes |

## Usage

### 1. Setup via Admin UI

1. Open `http://localhost:8080` in your browser
2. Enter the admin key (printed at startup or from `ADMIN_KEY` env)
3. Add upstream accounts (Accounts tab)
4. Optionally add SOCKS5 proxies (Proxies tab)
5. Generate employee API keys (API Keys tab)

### How to Get Token

#### Method 1: From claude.ai Web (Recommended)
1. Log in to https://claude.ai
2. Click avatar (top right) → **Settings**
3. Find **Claude Code** in left sidebar
4. Click **Generate session key** or **Create key**
5. Copy the `sk-ant-sid02-...` key

Paste it into the Token field in the admin dashboard, click "Verify Token" to confirm, then save.

#### Method 2: From Claude Code CLI
```bash
# Generate a long-lived token (~1 year)
claude setup-token

# Or read current login token (8h validity)
# macOS:
security find-generic-password -s 'Claude Code-credentials' -w | python3 -c \
  "import sys,json; d=json.load(sys.stdin); print(d['claudeAiOauth']['accessToken'])"

# Linux:
cat ~/.claude/.credentials.json | python3 -c \
  "import sys,json; d=json.load(sys.stdin); print(d['claudeAiOauth']['accessToken'])"
```

#### Token Types
| Format | Source | Validity | Needs Refresh Token |
|--------|--------|----------|-------------------|
| `sk-ant-sid02-` | claude.ai Web Settings | Long-lived | No |
| `sk-ant-oat01-` | CLI OAuth login | 8 hours | Yes (`sk-ant-ort01-`) |
| `sk-ant-api03-` | Anthropic Console | Permanent | No |

### 2. Employee API Calls

```bash
# Basic request
curl http://localhost:8080/v1/messages \
  -H "x-api-key: sk-gw-<employee-key>" \
  -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -d '{
    "model": "claude-sonnet-4-20250514",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello, Claude!"}]
  }'

# With session affinity (multi-turn)
curl http://localhost:8080/v1/messages \
  -H "x-api-key: sk-gw-<key>" \
  -H "session-id: conv-abc-123" \
  -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -d '{"model": "claude-sonnet-4-20250514", "max_tokens": 1024, "stream": true, ...}'

# Streaming (SSE) works transparently
```

### 3. Programmatic Usage (Python)

```python
from anthropic import Anthropic

client = Anthropic(
    base_url="http://localhost:8080",
    api_key="sk-gw-your-employee-key",
)

response = client.messages.create(
    model="claude-sonnet-4-20250514",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello!"}],
)
print(response.content[0].text)
```

## Admin API Reference

All admin endpoints require `Authorization: Bearer <ADMIN_KEY>`.

### Overview
| Method | Path | Description |
|--------|------|-------------|
| GET | `/admin/overview` | Dashboard statistics |

### Accounts
| Method | Path | Description |
|--------|------|-------------|
| GET | `/admin/accounts` | List all accounts |
| POST | `/admin/accounts` | Add account `{name, token, fingerprint?, rpm?, max_concur?}` |
| PUT | `/admin/accounts/{id}` | Update account fields |
| PUT | `/admin/accounts/{id}/status` | Change status `{status}` |
| PUT | `/admin/accounts/{id}/token` | Refresh token `{token, token_expiry?}` |
| DELETE | `/admin/accounts/{id}` | Delete account |

### API Keys
| Method | Path | Description |
|--------|------|-------------|
| GET | `/admin/keys` | List all keys |
| POST | `/admin/keys` | Generate key `{name, daily_limit?}` |
| PUT | `/admin/keys/{id}` | Update key `{enabled?, daily_limit?, name?}` |
| DELETE | `/admin/keys/{id}` | Delete key |

### Proxies
| Method | Path | Description |
|--------|------|-------------|
| GET | `/admin/proxies` | List all proxies |
| POST | `/admin/proxies` | Add proxy `{url, type?}` |
| POST | `/admin/proxies/batch` | Batch add `{urls, type?}` (newline-separated) |
| DELETE | `/admin/proxies/{id}` | Delete proxy |

### Statistics
| Method | Path | Description |
|--------|------|-------------|
| GET | `/admin/stats/daily?days=30` | Daily request stats |
| GET | `/admin/stats/hourly?hours=48` | Hourly request stats |
| GET | `/admin/stats/keys?days=30` | Per-key usage stats |
| GET | `/admin/stats/models?days=30` | Per-model usage stats |
| GET | `/admin/stats/pool` | Live pool status |
| GET | `/admin/stats/logs?limit=100` | Recent request logs |

### Operations
| Method | Path | Description |
|--------|------|-------------|
| POST | `/admin/ops/reload` | Force pool reload |
| POST | `/admin/ops/purge-logs` | Purge old logs `{days?}` |

### Health
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/health` | None | Health check endpoint |

## Testing

```bash
go test -v -count=1 ./...
```

## Project Structure

```
claude-gateway/
├── main.go           # Entry point, server setup, middleware chain
├── config.go         # Environment config with validation
├── util.go           # Leveled logger, HTTP helpers, context keys
├── store.go          # SQLite (WAL mode) with async batch log writer
├── pool.go           # Account pool: scheduling, affinity, cooldown, token refresh
├── auth.go           # Middleware: API key auth, account picker, CORS, admin auth
├── proxy.go          # Reverse proxy: header cleaning, SSE streaming, retry failover
├── admin.go          # Admin REST API + embedded web UI serving
├── web/
│   └── index.html    # Single-file dashboard (Tailwind + Alpine.js + Chart.js)
├── *_test.go         # Comprehensive tests (pool, store, proxy headers, auth)
├── Dockerfile        # Multi-stage Alpine build with health check
├── docker-compose.yml
├── .env.example      # Full config reference
└── README.md
```
