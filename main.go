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

// ── Pricing ─────────────────────────────────────────────────────

type ModelPrice struct {
	Input   float64 // per 1M tokens
	Output  float64 // per 1M tokens
	Cached  float64 // per 1M tokens
}

var modelPrices = map[string]ModelPrice{
	"glm-5.1":            {Input: 1.40, Output: 4.40, Cached: 0.26},
	"glm-5":              {Input: 1.00, Output: 3.20, Cached: 0.20},
	"deepseek-v4-pro":    {Input: 1.74, Output: 3.48, Cached: 0.0145},
	"deepseek-v4-flash":  {Input: 0.14, Output: 0.28, Cached: 0.0028},
	"kimi-k2.6":          {Input: 0.95, Output: 4.00, Cached: 0.16},
	"kimi-k2.5":          {Input: 0.60, Output: 3.00, Cached: 0.10},
	"mimo-v2.5":          {Input: 0.14, Output: 0.28, Cached: 0.0028},
	"mimo-v2.5-pro":      {Input: 1.74, Output: 3.48, Cached: 0.0145},
	"minimax-m3":         {Input: 0.30, Output: 1.20, Cached: 0.06},
	"minimax-m2.7":       {Input: 0.30, Output: 1.20, Cached: 0.06},
	"minimax-m2.5":       {Input: 0.30, Output: 1.20, Cached: 0.06},
	"qwen3.7-max":        {Input: 2.50, Output: 7.50, Cached: 0.50},
	"qwen3.7-plus":       {Input: 0.40, Output: 1.60, Cached: 0.04},
	"qwen3.6-plus":       {Input: 0.50, Output: 3.00, Cached: 0.05},
}

const (
	Limit5H   = 12.0
	LimitWeek = 30.0
	LimitMonth = 60.0
)

// ── Config ──────────────────────────────────────────────────────

type KeyEntry struct {
	Name      string `json:"name"`
	Key       string `json:"key"`
	Exhausted bool   `json:"exhausted,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	Note      string `json:"note,omitempty"`
}

type ModelSpend struct {
	Tokens  int64   `json:"tokens"`
	Dollars float64 `json:"dollars"`
}

type KeyUsage struct {
	Requests    int64                  `json:"requests"`
	TotalTokens int64                  `json:"total_tokens"`
	Dollars     float64                `json:"dollars"`
	LastUsed    time.Time              `json:"last_used,omitempty"`
	Models      map[string]*ModelSpend `json:"models,omitempty"`
	History     []SpendRecord          `json:"history,omitempty"`
}

type SpendRecord struct {
	Time    time.Time `json:"time"`
	Model   string    `json:"model"`
	Tokens  int64     `json:"tokens"`
	Dollars float64   `json:"dollars"`
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
	mux.HandleFunc("/api/keys", handleAPIKeys)
	mux.HandleFunc("/api/keys/", handleAPIKeyDetail)
	mux.HandleFunc("/api/stats", handleAPIStats)
	mux.HandleFunc("/api/models", handleAPIModels)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/usage", handleUsage)
	mux.HandleFunc("/dashboard", handleDashboard)
	mux.HandleFunc("/", handleProxy)

	go usageSaver()

	log.Printf("🔑 Load balancer running on http://localhost%s", listenAddr)
	log.Printf("   %d keys loaded | upstream: %s", len(cfg.Keys), upstreamURL)
	log.Printf("   Dashboard: http://localhost%s/dashboard", listenAddr)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

// ── Helpers ─────────────────────────────────────────────────────

func calcCost(model string, prompt, completion, cached int64) float64 {
	p, ok := modelPrices[model]
	if !ok {
		p = modelPrices["deepseek-v4-pro"]
	}
	inputCost := float64(prompt) * p.Input / 1e6
	outputCost := float64(completion) * p.Output / 1e6
	cachedCost := float64(cached) * p.Cached / 1e6
	return inputCost + outputCost + cachedCost
}

func periodSpend(keyName string, since time.Time) float64 {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	u, ok := cfg.Usage[keyName]
	if !ok {
		return 0
	}
	var total float64
	for _, r := range u.History {
		if r.Time.After(since) {
			total += r.Dollars
		}
	}
	return total
}

func totalPeriodSpend(since time.Time) float64 {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	var total float64
	for _, u := range cfg.Usage {
		for _, r := range u.History {
			if r.Time.After(since) {
				total += r.Dollars
			}
		}
	}
	return total
}

func cleanOldHistory() {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cutoff := time.Now().AddDate(0, 0, -35)
	for _, u := range cfg.Usage {
		var filtered []SpendRecord
		for _, r := range u.History {
			if r.Time.After(cutoff) {
				filtered = append(filtered, r)
			}
		}
		u.History = filtered
	}
}

// ── Key rotation ──────────────────────────────────────────────

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
		if !k.Exhausted && !k.Disabled {
			idxMu.Unlock()
			return k.Key
		}
		if keyIdx == start {
			for i := range cfg.Keys {
				cfg.Keys[i].Exhausted = false
			}
			log.Println("⚠️ All keys exhausted — resetting all")
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

// ── API handlers ────────────────────────────────────────────────

func handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	cfgMu.RLock()
	defer cfgMu.RUnlock()

	if r.Method == "GET" {
		type KeyResponse struct {
			KeyEntry
			Requests     int64                  `json:"requests"`
			TotalTokens  int64                  `json:"total_tokens"`
			Dollars      float64                `json:"dollars"`
			LastUsed     time.Time              `json:"last_used,omitempty"`
			Models       map[string]*ModelSpend `json:"models,omitempty"`
			Spend5H      float64                `json:"spend_5h"`
			SpendWeek    float64                `json:"spend_week"`
			SpendMonth   float64                `json:"spend_month"`
		}
		resp := make([]KeyResponse, 0, len(cfg.Keys))
		for _, k := range cfg.Keys {
			kr := KeyResponse{KeyEntry: k}
			if u, ok := cfg.Usage[k.Name]; ok {
				kr.Requests = u.Requests
				kr.TotalTokens = u.TotalTokens
				kr.Dollars = u.Dollars
				kr.LastUsed = u.LastUsed
				kr.Models = u.Models
			}
			now := time.Now()
			kr.Spend5H = periodSpend(k.Name, now.Add(-5*time.Hour))
			kr.SpendWeek = periodSpend(k.Name, now.AddDate(0, 0, -7))
			kr.SpendMonth = periodSpend(k.Name, now.AddDate(0, 0, -30))
			resp = append(resp, kr)
		}
		json.NewEncoder(w).Encode(resp)
		return
	}

	if r.Method == "POST" {
		var req struct {
			Name string `json:"name"`
			Key  string `json:"key"`
			Note string `json:"note,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
			return
		}
		if req.Name == "" || req.Key == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "name and key required"})
			return
		}
		for _, k := range cfg.Keys {
			if k.Name == req.Name {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{"error": "key name already exists"})
				return
			}
		}
		cfg.Keys = append(cfg.Keys, KeyEntry{Name: req.Name, Key: req.Key, Note: req.Note})
		cfg.Usage[req.Name] = &KeyUsage{Models: make(map[string]*ModelSpend)}
		triggerSave()
		json.NewEncoder(w).Encode(map[string]string{"status": "added"})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func handleAPIKeyDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	name := parts[3]

	cfgMu.Lock()
	defer cfgMu.Unlock()

	idx := -1
	for i, k := range cfg.Keys {
		if k.Name == name {
			idx = i
			break
		}
	}
	if idx == -1 {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "key not found"})
		return
	}

	if r.Method == "DELETE" {
		cfg.Keys = append(cfg.Keys[:idx], cfg.Keys[idx+1:]...)
		delete(cfg.Usage, name)
		triggerSave()
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
		return
	}

	if r.Method == "PUT" {
		var req struct {
			Disabled  bool   `json:"disabled,omitempty"`
			Exhausted bool   `json:"exhausted,omitempty"`
			Note      string `json:"note,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
			return
		}
		cfg.Keys[idx].Disabled = req.Disabled
		cfg.Keys[idx].Exhausted = req.Exhausted
		cfg.Keys[idx].Note = req.Note
		triggerSave()
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func handleAPIStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	cfgMu.RLock()
	defer cfgMu.RUnlock()

	now := time.Now()
	spend5H := totalPeriodSpend(now.Add(-5 * time.Hour))
	spendWeek := totalPeriodSpend(now.AddDate(0, 0, -7))
	spendMonth := totalPeriodSpend(now.AddDate(0, 0, -30))

	var totalDollars float64
	var totalRequests int64
	var totalTokens int64
	for _, u := range cfg.Usage {
		totalDollars += u.Dollars
		totalRequests += u.Requests
		totalTokens += u.TotalTokens
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"total_keys":    len(cfg.Keys),
		"active_keys":   countActive(),
		"exhausted_keys": countExhausted(),
		"disabled_keys": countDisabled(),
		"total_requests": totalRequests,
		"total_tokens":   totalTokens,
		"total_dollars":  totalDollars,
		"spend_5h":       spend5H,
		"spend_week":     spendWeek,
		"spend_month":    spendMonth,
		"remaining_5h":   Limit5H - spend5H,
		"remaining_week": LimitWeek - spendWeek,
		"remaining_month": LimitMonth - spendMonth,
		"percent_5h":     (spend5H / Limit5H) * 100,
		"percent_week":   (spendWeek / LimitWeek) * 100,
		"percent_month":  (spendMonth / LimitMonth) * 100,
	})
}

func handleAPIModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	cfgMu.RLock()
	defer cfgMu.RUnlock()

	models := make(map[string]map[string]interface{})
	for _, u := range cfg.Usage {
		for m, s := range u.Models {
			if _, ok := models[m]; !ok {
				models[m] = map[string]interface{}{"tokens": int64(0), "dollars": 0.0}
			}
			models[m]["tokens"] = models[m]["tokens"].(int64) + s.Tokens
			models[m]["dollars"] = models[m]["dollars"].(float64) + s.Dollars
		}
	}
	json.NewEncoder(w).Encode(models)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "healthy",
		"total_keys":  len(cfg.Keys),
		"active_keys": countActive(),
		"uptime":      time.Now().Format(time.RFC3339),
	})
}

func handleUsage(w http.ResponseWriter, r *http.Request) {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	stats := make([]map[string]interface{}, 0, len(cfg.Keys))
	for _, k := range cfg.Keys {
		s := map[string]interface{}{"name": k.Name, "exhausted": k.Exhausted, "disabled": k.Disabled}
		if u, ok := cfg.Usage[k.Name]; ok {
			s["requests"] = u.Requests
			s["total_tokens"] = u.TotalTokens
			s["dollars"] = u.Dollars
			s["last_used"] = u.LastUsed
		}
		stats = append(stats, s)
	}
	json.NewEncoder(w).Encode(stats)
}

func countActive() int {
	c := 0
	for _, k := range cfg.Keys {
		if !k.Exhausted && !k.Disabled {
			c++
		}
	}
	return c
}

func countExhausted() int {
	c := 0
	for _, k := range cfg.Keys {
		if k.Exhausted && !k.Disabled {
			c++
		}
	}
	return c
}

func countDisabled() int {
	c := 0
	for _, k := range cfg.Keys {
		if k.Disabled {
			c++
		}
	}
	return c
}

// ── Proxy ───────────────────────────────────────────────────────

var proxyClient = &http.Client{
	Timeout: 5 * time.Minute,
	Transport: &http.Transport{
		MaxIdleConns:     20,
		IdleConnTimeout:  90 * time.Second,
		DisableCompression: false,
	},
}

func handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" || r.URL.Path == "/usage" || r.URL.Path == "/dashboard" ||
		strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/static/") {
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"failed to read body"}`, http.StatusBadRequest)
		return
	}
	r.Body.Close()

	var reqBody map[string]interface{}
	modelName := "unknown"
	if json.Unmarshal(bodyBytes, &reqBody) == nil {
		if m, ok := reqBody["model"].(string); ok {
			modelName = m
		}
	}

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
			log.Printf("⚠️ Request failed with key '%s': %v", keyName, err)
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
				log.Printf("⚠️ Cloudflare blocked key '%s', retrying...", keyName)
				continue
			}
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var payload struct {
			Usage struct {
				PromptTokens        int64 `json:"prompt_tokens"`
				CompletionTokens    int64 `json:"completion_tokens"`
				TotalTokens         int64 `json:"total_tokens"`
				PromptTokensDetails struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBody, &payload) == nil && payload.Usage.TotalTokens > 0 {
			cost := calcCost(modelName, payload.Usage.PromptTokens, payload.Usage.CompletionTokens,
				payload.Usage.PromptTokensDetails.CachedTokens)

			cfgMu.Lock()
			if cfg.Usage == nil {
				cfg.Usage = make(map[string]*KeyUsage)
			}
			u, ok := cfg.Usage[keyName]
			if !ok {
				u = &KeyUsage{Models: make(map[string]*ModelSpend)}
				cfg.Usage[keyName] = u
			}
			u.Requests++
			u.TotalTokens += payload.Usage.TotalTokens
			u.Dollars += cost
			u.LastUsed = time.Now()
			if u.Models == nil {
				u.Models = make(map[string]*ModelSpend)
			}
			if u.Models[modelName] == nil {
				u.Models[modelName] = &ModelSpend{}
			}
			u.Models[modelName].Tokens += payload.Usage.TotalTokens
			u.Models[modelName].Dollars += cost
			u.History = append(u.History, SpendRecord{
				Time:    time.Now(),
				Model:   modelName,
				Tokens:  payload.Usage.TotalTokens,
				Dollars: cost,
			})
			cfgMu.Unlock()
			triggerSave()
			cleanOldHistory()
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

// ── Dashboard ───────────────────────────────────────────────────

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data, err := os.ReadFile("dashboard.html")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Dashboard not found. Make sure dashboard.html is in the same directory."))
		return
	}
	w.Write(data)
}

// ── Persistence ───────────────────────────────────────────────

func triggerSave() {
	select {
	case saveCh <- struct{}{}:
	default:
	}
}

func usageSaver() {
	for range saveCh {
		time.Sleep(saveDebounce * time.Duration(len(saveCh)+1))
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
	if cfg.Usage == nil {
		cfg.Usage = make(map[string]*KeyUsage)
	}
	for _, k := range cfg.Keys {
		if _, ok := cfg.Usage[k.Name]; !ok {
			cfg.Usage[k.Name] = &KeyUsage{Models: make(map[string]*ModelSpend)}
		}
	}
}

func saveConfig() {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
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
