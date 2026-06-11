# OpenCode Proxy Balancer

A local load-balancing proxy for OpenCode Go with key rotation, real-time dollar tracking, and a management dashboard.

## What it does

- **Multi-key routing** — round-robin load balancing across multiple API keys
- **Auto-failover** — when a key hits rate limits, it marks it exhausted and retries with the next
- **Dollar tracking** — calculates exact cost per request using OpenCode's pricing table
- **Budget monitoring** — tracks 5-hour, weekly, and monthly spending against limits
- **Management dashboard** — add/remove keys, see usage, monitor costs in real-time

## Architecture

```
OpenCode → localhost:8320 (proxy) → opencode.ai API
                │
                ├── Key rotation (round-robin)
                ├── Usage tracking (tokens, dollars, models)
                ├── Budget monitoring ($12/5h, $30/wk, $60/mo)
                └── Dashboard (http://localhost:8320/dashboard)
```

## Quick Start

```bash
# Clone the repo
git clone https://github.com/Sid8050/OpencodeProxyBalancer.git
cd OpencodeProxyBalancer

# Create your keys config
cp keys.json.template keys.json
# Edit keys.json — add your API key(s)

# Build and start
go build -o proxy main.go
./proxy
```

Then open **http://localhost:8320/dashboard** in your browser.

## keys.json Configuration

```json
{
  "keys": [
    {
      "name": "account-1",
      "key": "sk-your-api-key-here",
      "note": "Primary account"
    },
    {
      "name": "account-2",
      "key": "sk-your-second-key",
      "note": "Backup account"
    }
  ],
  "usage": {}
}
```

**`keys.json` is gitignored** — it never leaves your machine.

## OpenCode Configuration

Point your OpenCode provider at the proxy. In `opencode.json`:

```json
{
  "provider": {
    "opencode-go-zen": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "OpenCode Go (via proxy)",
      "options": {
        "baseURL": "http://localhost:8320",
        "apiKey": "proxy-handled"
      },
      "models": {
        "deepseek-v4-pro": { "id": "deepseek-v4-pro" },
        "kimi-k2.6":       { "id": "kimi-k2.6" },
        "glm-5.1":          { "id": "glm-5.1" }
      }
    }
  },
  "agent": {
    "backend": {
      "model": "opencode-go-zen/deepseek-v4-pro"
    },
    "frontend": {
      "model": "opencode-go-zen/kimi-k2.6"
    },
    "think-deep": {
      "model": "opencode-go-zen/glm-5.1"
    }
  }
}
```

## macOS Integration

### Auto-start on login (LaunchAgent)

```bash
./install-service.sh
```

The proxy starts automatically when you log in and stays running in the background.

### Manual control

Double-click **OpenCode Proxy.app** to start/stop with a click.

To stop the service: `launchctl unload ~/Library/LaunchAgents/com.opencode.proxy.plist`  
To start again: `launchctl load ~/Library/LaunchAgents/com.opencode.proxy.plist`

## Dashboard

Open **http://localhost:8320/dashboard** for a full management interface:

- **Budget cards** — remaining 5-hour, weekly, and monthly budget
- **Key management** — add, remove, enable/disable, mark exhausted
- **Cost tracking** — per-key and per-model dollar spend
- **Model breakdown** — which models are consuming your budget
- **Auto-refresh** — updates every 3 seconds

## API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/keys` | GET | List all keys with usage stats |
| `/api/keys` | POST | Add a new key |
| `/api/keys/:name` | PUT | Update key settings |
| `/api/keys/:name` | DELETE | Remove a key |
| `/api/stats` | GET | Overall spending stats |
| `/api/models` | GET | Per-model cost breakdown |
| `/health` | GET | Health check |

## Tracked Models & Pricing

The proxy calculates cost using the official OpenCode Go pricing:

| Model | Input ($/1M) | Output ($/1M) |
|-------|-------------|---------------|
| DeepSeek V4 Pro | $1.74 | $3.48 |
| GLM-5.1 | $1.40 | $4.40 |
| Kimi K2.6 | $0.95 | $4.00 |
| DeepSeek V4 Flash | $0.14 | $0.28 |
| + 11 more models | | |

Full pricing table in `main.go` modelPrices map.

## Budget Limits

The proxy tracks against OpenCode's default limits:

| Period | Limit | Source |
|--------|-------|--------|
| Rolling 5-hour | $12.00 | Resets automatically |
| Weekly | $30.00 | Rolling 7 days |
| Monthly | $60.00 | Rolling 30 days |

## Security

- `keys.json` is **gitignored** — never commit it
- Keys are stored locally with `0600` permissions
- The proxy runs on `localhost` only — not exposed to the network
- No keys or secrets appear in any tracked files

## Troubleshooting

**Dashboard shows $0.00 for all costs?**  
Costs under $0.01 are displayed with 4+ decimal places. Values like $0.0004 are valid for small token counts.

**Proxy shows fewer requests than expected?**  
Only requests through the `opencode-go-zen` provider are tracked. The built-in `opencode-go` provider bypasses the proxy. Make sure your agents use `opencode-go-zen/` models.

**Port 8320 already in use?**  
```bash
lsof -ti:8320 | xargs kill -9
```

**Dashboard won't load?**  
Make sure the proxy is running: `curl http://localhost:8320/health`

## License

MIT
