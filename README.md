# OpenCode Proxy Balancer

A local load-balancing proxy for [OpenCode Go](https://opencode.ai) with multi-key rotation, real-time dollar tracking, and a management dashboard.

## What it does

- **Multi-key routing** — round-robin load balancing across multiple API keys
- **Auto-failover** — exhausted keys (429/402) are skipped and retried with the next key
- **Streaming support** — handles both standard JSON and SSE streaming responses from OpenCode
- **Dollar tracking** — calculates exact cost per request using OpenCode's pricing table (14 models)
- **Budget monitoring** — tracks 5-hour ($12), weekly ($30), and monthly ($60) rolling budgets
- **Management dashboard** — add/remove keys, toggle status, view per-key and per-model costs

## Architecture

```
OpenCode → localhost:8320 (proxy) → opencode.ai/zen/go/v1
                │
                ├── Round-robin key rotation
                ├── Usage tracking (JSON + SSE parsing)
                ├── Dollar cost calculation (14 models)
                ├── Budget enforcement ($12/5h, $30/wk, $60/mo)
                └── Dashboard (http://localhost:8320/dashboard)
```

## Quick Start

```bash
# Clone
git clone https://github.com/Sid8050/OpencodeProxyBalancer.git
cd OpencodeProxyBalancer

# Create keys config (gitignored — never leaves your machine)
echo '{
  "keys": [
    {"name": "account-1", "key": "sk-your-api-key", "note": "Primary"},
    {"name": "account-2", "key": "sk-another-key",  "note": "Backup"}
  ],
  "usage": {}
}' > keys.json

# Build and start
go build -o proxy .
./proxy
```

Open **http://localhost:8320/dashboard** in your browser. The dashboard updates every 3 seconds.

## keys.json

```json
{
  "keys": [
    {"name": "account-1", "key": "sk-...", "note": "Primary account"},
    {"name": "account-2", "key": "sk-...", "note": "Backup", "disabled": true}
  ],
  "usage": {
    "account-1": {
      "requests": 42,
      "total_tokens": 123456,
      "dollars": 0.215,
      "last_used": "2026-06-13T08:59:13+03:00",
      "models": {
        "deepseek-v4-pro": {"tokens": 90000, "dollars": 0.157},
        "kimi-k2.6": {"tokens": 33456, "dollars": 0.058}
      },
      "history": [...]
    }
  }
}
```

- **`keys.json` is gitignored** — never committed to version control
- Each key entry: `name` (display label), `key` (API key), optional `disabled`, `exhausted`, `note`
- The `usage` section is auto-managed by the proxy — don't edit it manually
- You can add/remove keys from the dashboard UI or the API without restarting

## OpenCode Configuration

In `~/.config/opencode/opencode.json`, add the provider:

```json
{
  "enabled_providers": ["opencode-go-zen"],
  "provider": {
    "opencode-go-zen": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "OpenCode Go Zen",
      "options": {
        "baseURL": "http://localhost:8320",
        "apiKey": "{file:/Users/<you>/.config/opencode/zen.key}",
        "timeout": 300000
      },
      "models": {
        "deepseek-v4-pro":  { "id": "deepseek-v4-pro" },
        "kimi-k2.6":        { "id": "kimi-k2.6" },
        "glm-5.1":          { "id": "glm-5.1" }
      }
    }
  },
  "agent": {
    "backend":   { "model": "opencode-go-zen/deepseek-v4-pro" },
    "frontend":  { "model": "opencode-go-zen/kimi-k2.6" },
    "think-deep": { "model": "opencode-go-zen/glm-5.1" }
  }
}
```

> **Important**: Use the `opencode-go-zen` provider — the built-in `opencode-go` provider cannot be intercepted by the proxy. The proxy substitutes its own rotating API key, so the `apiKey` value in your config can be anything (it's replaced at the proxy level).

## macOS Auto-Start

### LaunchAgent (login start)

```bash
./install-service.sh
```

Installs `com.opencode.proxy.plist` in `~/Library/LaunchAgents/`. The proxy starts automatically when you log in.

Check if running:
```bash
launchctl list | grep opencode
```

### Manual control

Double-click **Proxy Control.command** for a simple Terminal menu:

```
═══════════════════════════════════
   OpenCode Proxy — Control Panel
═══════════════════════════════════

  Status: ✅ RUNNING  (port 8320)

  1) Open Dashboard
  2) Stop Proxy

  0) Quit
```

> **macOS Gatekeeper note**: If the LaunchAgent fails with exit code 78, the binary may need code signing. In Terminal: `codesign -s - -f /path/to/proxy` and retry. The `.command` file bypasses Gatekeeper since it runs in Terminal.

## Dashboard

Open **http://localhost:8320/dashboard** for the management interface:

- **Budget cards** — remaining 5-hour, weekly, monthly budget with progress bars
- **Key management** — add, remove, enable/disable, mark exhausted
- **Cost tracking** — per-key totals with time-period breakdowns (5h/week/month)
- **Model tags** — each key's usage broken down by model
- **Auto-refresh** — updates every 3 seconds
- **Light theme** — professional design with system fonts and blue accents
- **Decimal precision** — costs < $0.001 show 6 decimal places, < $0.01 show 4, < $1 show 3

## API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/keys` | GET | List keys with per-key usage, 5h/week/month spend |
| `/api/keys` | POST | Add a new key (`{"name":"...","key":"sk-..."}`) |
| `/api/keys/:name` | PUT | Toggle `disabled`/`exhausted`, update `note` |
| `/api/keys/:name` | DELETE | Remove key and its usage data |
| `/api/stats` | GET | Totals: spend, tokens, requests, budget percentages |
| `/api/models` | GET | Aggregated per-model token counts and dollar costs |
| `/health` | GET | Health check (`{"status":"healthy","active_keys":2}`) |
| `/usage` | GET | Per-key summary (simpler than `/api/keys`) |

## Tracked Models & Pricing

The proxy calculates cost using OpenCode Go Zen pricing (per 1M tokens):

| Model | Input | Output | Cached |
|-------|-------|--------|--------|
| DeepSeek V4 Pro | $1.74 | $3.48 | $0.0145 |
| DeepSeek V4 Flash | $0.14 | $0.28 | $0.0028 |
| GLM-5.1 | $1.40 | $4.40 | $0.26 |
| GLM-5 | $1.00 | $3.20 | $0.20 |
| Kimi K2.6 | $0.95 | $4.00 | $0.16 |
| Kimi K2.5 | $0.60 | $3.00 | $0.10 |
| Mimo V2.5 Pro | $1.74 | $3.48 | $0.0145 |
| Mimo V2.5 | $0.14 | $0.28 | $0.0028 |
| MiniMax M3 | $0.30 | $1.20 | $0.06 |
| MiniMax M2.7 | $0.30 | $1.20 | $0.06 |
| MiniMax M2.5 | $0.30 | $1.20 | $0.06 |
| Qwen 3.7 Max | $2.50 | $7.50 | $0.50 |
| Qwen 3.7 Plus | $0.40 | $1.60 | $0.04 |
| Qwen 3.6 Plus | $0.50 | $3.00 | $0.05 |

## Budget Limits

| Period | Limit | Window |
|--------|-------|--------|
| 5-hour | $12.00 | Rolling 5 hours |
| Weekly | $30.00 | Rolling 7 days |
| Monthly | $60.00 | Rolling 30 days |

When all keys are exhausted (all hit 429), the proxy resets all exhaustion flags and continues.

## Security

- `keys.json` is **gitignored** — never leave your machine
- Keys stored with `0600` permissions
- Runs on `localhost` only — not exposed to the network
- Proxy overwrites the incoming `Authorization` header with rotating keys from `keys.json`
- API key dumps are never logged (only masked `sk-...XXXX`)

## Troubleshooting

### Dashboard shows $0.00 for everything
Costs under $0.01 are displayed with 4–6 decimal places. Values like `$0.0004` are valid for small token counts.

### Some keys show 0 requests despite getting traffic
The proxy handles SSE streaming responses from OpenCode. Make sure you're running the latest version which includes SSE parsing. Check `proxy.log` for `📊` entries (successful tracking) or `⚠️ [skip-track]` messages (tracking skipped — indicates a format issue).

### Port 8320 already in use
```bash
lsof -ti:8320 | xargs kill -9
```

### Proxy won't start via LaunchAgent
macOS Gatekeeper may block unsigned binaries. In Terminal:
```bash
codesign -s - -f /path/to/proxy
```
Then reload: `launchctl load ~/Library/LaunchAgents/com.opencode.proxy.plist`

### Requests not routing through the proxy
Verify your agents use the `opencode-go-zen/` prefix, NOT `opencode-go/`. The built-in provider bypasses the proxy.

### All costs showing as $0.00
Open `http://localhost:8320/health` — if it returns a response, the proxy is running. Check `keys.json` is properly formatted and contains valid keys.

## License

MIT
