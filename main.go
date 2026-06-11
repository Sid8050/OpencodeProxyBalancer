package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ── Config / State ──────────────────────────────────────────────

type KeyEntry struct {
	Name      string `json:"name"`
	Key       string `json:"key"`
	Exhausted bool   `json:"exhausted,omitempty"`
}

type KeyUsage struct {
	Requests    int64     `json:"requests"`
	TotalTokens int64     `json:"total_tokens"`
	LastUsed    time.Time `json:"last_used,omitempty"`
}

type Config struct {
	Keys  []KeyEntry           `json:"keys"`
	Usage map[string]*KeyUsage `json:"usage,omitempty"`
}

const (
	configPath   = "keys.json"
	upstreamURL  = "https://opencode.ai/zen/go/v1"
	listenAddr   = ":8320"
	saveDebounce = 2 * time.Second
)

var (
	cfg     Config
	cfgMu   sync.RWMutex
	keyIdx  int
	idxMu   sync.Mutex
	saveCh  = make(chan struct{}, 1)
)

// ── Main ────────────────────────────────────────────────────────

func main() {
	loadConfig()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/usage", handleUsage)
	mux.HandleFunc("/dashboard", handleDashboard)
	mux.HandleFunc("/", handleProxy)

	// Background usage saver
	go usageSaver()

	log.Printf("🔑 Load balancer running on http://localhost%s", listenAddr)
	log.Printf("   %d keys loaded | upstream: %s", len(cfg.Keys), upstreamURL)
	log.Printf("   Dashboard: http://localhost%s/dashboard", listenAddr)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

// ── Key rotation ────────────────────────────────────────────────

func pickKey() string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()

	if len(cfg.Keys) == 0 {
		return ""
	}

	idxMu.Lock()
	start := keyIdx
	for {
		k := &cfg.Keys[keyIdx]
		keyIdx = (keyIdx + 1) % len(cfg.Keys)
		if !k.Exhausted {
			idxMu.Unlock()
			return k.Key
		}
		if keyIdx == start {
			// All exhausted — reset and use first
			for i := range cfg.Keys {
				cfg.Keys[i].Exhausted = false
			}
			log.Println("⚠️  All keys exhausted — resetting all")
			idxMu.Unlock()
			return cfg.Keys[0].Key
		}
	}
}

func markExhausted(name string) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	for i := range cfg.Keys {
		if cfg.Keys[i].Name == name {
			cfg.Keys[i].Exhausted = true
			log.Printf("🚫 Key '%s' marked exhausted", name)
			triggerSave()
			return
		}
	}
}

// ── Proxy handler (manual forwarding with key rotation) ─────────

var proxyClient = &http.Client{
	Timeout: 5 * time.Minute,
	Transport: &http.Transport{
		MaxIdleConns:        20,
		IdleConnTimeout:      90 * time.Second,
		DisableCompression:   false,
	},
}

func handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" || r.URL.Path == "/usage" || r.URL.Path == "/dashboard" {
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"failed to read body"}`, http.StatusBadRequest)
		return
	}
	r.Body.Close()

	targetURL := upstreamURL + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	maxRetries := len(cfg.Keys) + 1
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		key := pickKey()
		if key == "" {
			http.Error(w, `{"error":"no API keys configured"}`, http.StatusServiceUnavailable)
			return
		}

		keyName := keyNameForKey(key)

		req, err := http.NewRequest(r.Method, targetURL, bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, `{"error":"failed to create request"}`, http.StatusInternalServerError)
			return
		}

		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "OpenCodeProxy/1.0")
		req.Header.Set("Accept", "application/json")

		resp, err := proxyClient.Do(req)
		if err != nil {
			lastErr = err
			log.Printf("⚠️  Request failed with key '%s': %v", keyName, err)
			continue
		}

		if resp.StatusCode == 429 || resp.StatusCode == 402 {
			resp.Body.Close()
			markExhausted(keyName)
			log.Printf("🔄 Key '%s' exhausted (HTTP %d), retrying...", keyName, resp.StatusCode)
			continue
		}

		if resp.StatusCode == 403 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(body), "cloudflare") {
				log.Printf("⚠️  Cloudflare blocked key '%s', retrying...", keyName)
				continue
			}
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var payload struct {
			Usage struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBody, &payload) == nil && payload.Usage.TotalTokens > 0 {
			cfgMu.Lock()
			if cfg.Usage == nil {
				cfg.Usage = make(map[string]*KeyUsage)
			}
			u, ok := cfg.Usage[keyName]
			if !ok {
				u = &KeyUsage{}
				cfg.Usage[keyName] = u
			}
			u.Requests++
			u.TotalTokens += payload.Usage.TotalTokens
			u.LastUsed = time.Now()
			cfgMu.Unlock()
			triggerSave()
		}

		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	log.Printf("❌ All keys exhausted or failed. Last error: %v", lastErr)
	http.Error(w, `{"error":"all API keys exhausted"}`, http.StatusServiceUnavailable)
}

func keyNameForKey(key string) string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	for _, k := range cfg.Keys {
		if k.Key == key {
			return k.Name
		}
	}
	return "unknown"
}

// ── Handlers ────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, r *http.Request) {
	cfgMu.RLock()
	totalKeys := len(cfg.Keys)
	active := 0
	for _, k := range cfg.Keys {
		if !k.Exhausted {
			active++
		}
	}
	cfgMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "healthy",
		"total_keys":  totalKeys,
		"active_keys": active,
		"uptime":      time.Now().Format(time.RFC3339),
	})
}

func handleUsage(w http.ResponseWriter, r *http.Request) {
	cfgMu.RLock()
	defer cfgMu.RUnlock()

	type KeyStats struct {
		Name        string    `json:"name"`
		Exhausted   bool      `json:"exhausted"`
		Requests    int64     `json:"requests"`
		TotalTokens int64     `json:"total_tokens"`
		LastUsed    time.Time `json:"last_used,omitempty"`
	}

	stats := make([]KeyStats, 0, len(cfg.Keys))
	for _, k := range cfg.Keys {
		s := KeyStats{Name: k.Name, Exhausted: k.Exhausted}
		if u, ok := cfg.Usage[k.Name]; ok {
			s.Requests = u.Requests
			s.TotalTokens = u.TotalTokens
			s.LastUsed = u.LastUsed
		}
		stats = append(stats, s)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(dashboardHTML))
}

// ── Persistence ─────────────────────────────────────────────────

func triggerSave() {
	select {
	case saveCh <- struct{}{}:
	default:
	}
}

func usageSaver() {
	for range saveCh {
		time.Sleep(saveDebounce * time.Duration(len(saveCh)+1)) // batch

		// Drain channel
		drain:
		for {
			select {
			case <-saveCh:
			default:
				break drain
			}
		}

		saveConfig()
	}
}

func loadConfig() {
	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Printf("No %s found — create one with your API keys", configPath)
		return
	}

	cfgMu.Lock()
	defer cfgMu.Unlock()

	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("Error parsing %s: %v", configPath, err)
		return
	}

	// Ensure usage map initialized
	if cfg.Usage == nil {
		cfg.Usage = make(map[string]*KeyUsage)
	}

	// Fill missing usage entries
	for _, k := range cfg.Keys {
		if _, ok := cfg.Usage[k.Name]; !ok {
			cfg.Usage[k.Name] = &KeyUsage{}
		}
	}
}

func saveConfig() {
	cfgMu.RLock()
	defer cfgMu.RUnlock()

	// Sort keys for consistent output
	sort.Slice(cfg.Keys, func(i, j int) bool {
		return cfg.Keys[i].Name < cfg.Keys[j].Name
	})

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Printf("Error marshaling config: %v", err)
		return
	}

	if err := os.WriteFile(configPath, data, 0600); err != nil {
		log.Printf("Error saving config: %v", err)
	}
}

// ── Dashboard HTML ──────────────────────────────────────────────

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Key Usage Dashboard</title>
<style>
  :root { --bg:#0a0a0f; --card:rgba(255,255,255,0.04); --text:#e4e4e7; --muted:#71717a; --green:#22c55e; --red:#ef4444; --amber:#f59e0b; --accent:#22d3ee; }
  *{margin:0;padding:0;box-sizing:border-box}
  body{font-family:system-ui,sans-serif;background:var(--bg);color:var(--text);min-height:100vh;padding:40px 20px}
  .container{max-width:700px;margin:0 auto}
  h1{font-size:1.8rem;margin-bottom:8px;letter-spacing:-0.02em}
  .subtitle{color:var(--muted);font-size:14px;margin-bottom:32px}
  .summary{display:flex;gap:16px;margin-bottom:32px}
  .summary-card{flex:1;background:var(--card);border-radius:12px;padding:20px;text-align:center;border:1px solid rgba(255,255,255,0.06)}
  .summary-card .value{font-size:2rem;font-weight:700}
  .summary-card .label{font-size:12px;color:var(--muted);margin-top:4px;text-transform:uppercase;letter-spacing:0.05em}
  .key-card{background:var(--card);border-radius:12px;padding:20px;margin-bottom:12px;border:1px solid rgba(255,255,255,0.06);display:flex;align-items:center;gap:16px;transition:all 0.2s}
  .key-card.exhausted{opacity:0.45;border-color:rgba(239,68,68,0.2)}
  .status-dot{width:10px;height:10px;border-radius:50%;flex-shrink:0}
  .status-dot.active{background:var(--green);box-shadow:0 0 8px rgba(34,197,94,0.4)}
  .status-dot.exhausted{background:var(--red)}
  .key-info{flex:1}
  .key-name{font-weight:600;font-size:15px}
  .key-key{font-size:12px;color:var(--muted);font-family:monospace;margin-top:2px}
  .key-stats{text-align:right}
  .key-stats .tokens{font-size:14px;font-weight:600}
  .key-stats .requests{font-size:12px;color:var(--muted)}
  .key-stats .last{font-size:11px;color:var(--muted)}
  .badge{display:inline-block;padding:2px 8px;border-radius:6px;font-size:11px;font-weight:600}
  .badge-active{background:rgba(34,197,94,0.15);color:var(--green)}
  .badge-exhausted{background:rgba(239,68,68,0.15);color:var(--red)}
  .refresh{display:inline-flex;align-items:center;gap:6px;color:var(--accent);cursor:pointer;font-size:13px;background:none;border:none;margin-bottom:24px}
  .refresh:hover{text-decoration:underline}
  @media(max-width:500px){.summary{flex-direction:column}.key-card{flex-wrap:wrap}}
  .empty-state{text-align:center;padding:60px 20px;color:var(--muted)}
  .empty-state .icon{font-size:48px;margin-bottom:12px}
</style>
</head>
<body>
<div class="container">
  <h1>🔑 Key Balancer</h1>
  <p class="subtitle">OpenCode Go Zen — load balanced across multiple keys</p>
  <button class="refresh" onclick="location.reload()">↻ Refresh</button>
  <div class="summary">
    <div class="summary-card">
      <div class="value" id="totalKeys">-</div>
      <div class="label">Total Keys</div>
    </div>
    <div class="summary-card">
      <div class="value" id="activeKeys">-</div>
      <div class="label">Active</div>
    </div>
    <div class="summary-card">
      <div class="value" id="totalTokens">-</div>
      <div class="label">Total Tokens</div>
    </div>
    <div class="summary-card">
      <div class="value" id="totalRequests">-</div>
      <div class="label">Requests</div>
    </div>
  </div>
  <div id="keyList"></div>
  <div class="empty-state" id="emptyState" style="display:none">
    <div class="icon">📭</div>
    <p>No keys configured. Add them to keys.json</p>
  </div>
</div>
<script>
async function load() {
  const res = await fetch('/usage');
  const keys = await res.json();
  const total = keys.length;
  const active = keys.filter(k => !k.exhausted).length;
  const totalTokens = keys.reduce((s,k) => s + k.total_tokens, 0);
  const totalReqs = keys.reduce((s,k) => s + k.requests, 0);

  document.getElementById('totalKeys').textContent = total;
  document.getElementById('activeKeys').textContent = active;
  document.getElementById('totalTokens').textContent = totalTokens.toLocaleString();
  document.getElementById('totalRequests').textContent = totalReqs.toLocaleString();

  const list = document.getElementById('keyList');
  if (keys.length === 0) {
    document.getElementById('emptyState').style.display = 'block';
    return;
  }
  document.getElementById('emptyState').style.display = 'none';

  list.innerHTML = keys.map(k => {
    const cls = k.exhausted ? 'exhausted' : '';
    const dot = k.exhausted ? 'exhausted' : 'active';
    const badge = k.exhausted ? '<span class="badge badge-exhausted">exhausted</span>' : '<span class="badge badge-active">active</span>';
    const masked = k.name.length > 1 ? 'sk-••••' + k.name.slice(-4) : k.name;
    const lastUsed = k.last_used ? new Date(k.last_used).toLocaleString() : 'never';
    return '<div class="key-card ' + cls + '">' +
      '<div class="status-dot ' + dot + '"></div>' +
      '<div class="key-info"><div class="key-name">' + k.name + ' ' + badge + '</div>' +
      '<div class="key-key">' + masked + '</div></div>' +
      '<div class="key-stats">' +
      '<div class="tokens">' + k.total_tokens.toLocaleString() + ' tokens</div>' +
      '<div class="requests">' + k.requests.toLocaleString() + ' requests</div>' +
      '<div class="last">' + lastUsed + '</div>' +
      '</div></div>';
  }).join('');
}
load();
setInterval(load, 5000);
</script>
</body>
</html>`
