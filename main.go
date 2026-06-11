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

// Config structures
type KeyEntry struct {
	Name      string `json:"name"`
	Key       string `json:"key"`
	Exhausted bool   `json:"exhausted,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	Limit     int64  `json:"limit,omitempty"`
	Note      string `json:"note,omitempty"`
}

type KeyUsage struct {
	Requests    int64            `json:"requests"`
	TotalTokens int64            `json:"total_tokens"`
	LastUsed    time.Time        `json:"last_used,omitempty"`
	Models      map[string]int64 `json:"models,omitempty"`
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

func main() {
	loadConfig()

	mux := http.NewServeMux()

	// API endpoints
	mux.HandleFunc("/api/keys", handleAPIKeys)
	mux.HandleFunc("/api/keys/", handleAPIKeyDetail)
	mux.HandleFunc("/api/stats", handleAPIStats)
	mux.HandleFunc("/api/models", handleAPIModels)

	// Legacy endpoints
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/usage", handleUsage)

	// Dashboard
	mux.HandleFunc("/dashboard", handleDashboard)

	// Proxy
	mux.HandleFunc("/", handleProxy)

	go usageSaver()

	log.Printf("🔑 Load balancer running on http://localhost%s", listenAddr)
	log.Printf("   %d keys loaded | upstream: %s", len(cfg.Keys), upstreamURL)
	log.Printf("   Dashboard: http://localhost%s/dashboard", listenAddr)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

// Key rotation
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

// API handlers
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
			Requests    int64            `json:"requests"`
			TotalTokens int64            `json:"total_tokens"`
			LastUsed    time.Time        `json:"last_used,omitempty"`
			Models      map[string]int64 `json:"models,omitempty"`
			Remaining   int64            `json:"remaining,omitempty"`
			Percent     float64          `json:"percent,omitempty"`
		}

		resp := make([]KeyResponse, 0, len(cfg.Keys))
		for _, k := range cfg.Keys {
			kr := KeyResponse{KeyEntry: k}
			if u, ok := cfg.Usage[k.Name]; ok {
				kr.Requests = u.Requests
				kr.TotalTokens = u.TotalTokens
				kr.LastUsed = u.LastUsed
				kr.Models = u.Models
			}
			if k.Limit > 0 {
				kr.Remaining = k.Limit - kr.TotalTokens
				if kr.Remaining < 0 {
					kr.Remaining = 0
				}
				kr.Percent = float64(kr.TotalTokens) / float64(k.Limit) * 100
			}
			resp = append(resp, kr)
		}
		json.NewEncoder(w).Encode(resp)
		return
	}

	if r.Method == "POST" {
		var req struct {
			Name  string `json:"name"`
			Key   string `json:"key"`
			Limit int64  `json:"limit,omitempty"`
			Note  string `json:"note,omitempty"`
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
		cfg.Keys = append(cfg.Keys, KeyEntry{
			Name:  req.Name,
			Key:   req.Key,
			Limit: req.Limit,
			Note:  req.Note,
		})
		cfg.Usage[req.Name] = &KeyUsage{Models: make(map[string]int64)}
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
			Disabled bool   `json:"disabled,omitempty"`
			Exhausted bool  `json:"exhausted,omitempty"`
			Limit    int64  `json:"limit,omitempty"`
			Note     string `json:"note,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
			return
		}
		cfg.Keys[idx].Disabled = req.Disabled
		cfg.Keys[idx].Exhausted = req.Exhausted
		cfg.Keys[idx].Limit = req.Limit
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

	totalKeys := len(cfg.Keys)
	active := 0
	disabled := 0
	var totalTokens int64
	var totalRequests int64
	var totalLimit int64
	var usedLimit int64

	for _, k := range cfg.Keys {
		if !k.Exhausted && !k.Disabled {
			active++
		}
		if k.Disabled {
			disabled++
		}
		if k.Limit > 0 {
			totalLimit += k.Limit
		}
	}
	for _, u := range cfg.Usage {
		totalTokens += u.TotalTokens
		totalRequests += u.Requests
		usedLimit += u.TotalTokens
	}

	var percent float64
	if totalLimit > 0 {
		percent = float64(usedLimit) / float64(totalLimit) * 100
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"total_keys":     totalKeys,
		"active_keys":    active,
		"disabled_keys":  disabled,
		"exhausted_keys": totalKeys - active - disabled,
		"total_tokens":   totalTokens,
		"total_requests": totalRequests,
		"total_limit":    totalLimit,
		"used_limit":     usedLimit,
		"percent":        percent,
	})
}

func handleAPIModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	cfgMu.RLock()
	defer cfgMu.RUnlock()

	models := make(map[string]int64)
	for _, u := range cfg.Usage {
		for m, c := range u.Models {
			models[m] += c
		}
	}
	json.NewEncoder(w).Encode(models)
}

// Legacy handlers
func handleHealth(w http.ResponseWriter, r *http.Request) {
	cfgMu.RLock()
	totalKeys := len(cfg.Keys)
	active := 0
	for _, k := range cfg.Keys {
		if !k.Exhausted && !k.Disabled {
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
		Disabled    bool      `json:"disabled"`
		Requests    int64     `json:"requests"`
		TotalTokens int64     `json:"total_tokens"`
		LastUsed    time.Time `json:"last_used,omitempty"`
	}

	stats := make([]KeyStats, 0, len(cfg.Keys))
	for _, k := range cfg.Keys {
		s := KeyStats{Name: k.Name, Exhausted: k.Exhausted, Disabled: k.Disabled}
		if u, ok := cfg.Usage[k.Name]; ok {
			s.Requests = u.Requests
			s.TotalTokens = u.TotalTokens
			s.LastUsed = u.LastUsed
		}
		stats = append(stats, s)
	}
	json.NewEncoder(w).Encode(stats)
}

// Proxy
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
				u = &KeyUsage{Models: make(map[string]int64)}
				cfg.Usage[keyName] = u
			}
			u.Requests++
			u.TotalTokens += payload.Usage.TotalTokens
			u.LastUsed = time.Now()
			if u.Models == nil {
				u.Models = make(map[string]int64)
			}
			u.Models[modelName] += payload.Usage.TotalTokens
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

// Dashboard
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

// Persistence
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
			cfg.Usage[k.Name] = &KeyUsage{Models: make(map[string]int64)}
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

// Dashboard HTML


