package tunnel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// AdminCORS sets CORS headers for admin endpoints.
func AdminCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Content-Type", "application/json")
}

// HandleAdminConfig handles GET/PUT for system settings.
func (b *Base) HandleAdminConfig(w http.ResponseWriter, r *http.Request) {
	AdminCORS(w)
	if r.Method == http.MethodOptions {
		return
	}

	switch r.Method {
	case http.MethodGet:
		b.CfgMu.RLock()
		cfg := b.Cfg
		b.CfgMu.RUnlock()

		b.CfgMu.RLock()
		authCfg := b.Auth
		b.CfgMu.RUnlock()

		json.NewEncoder(w).Encode(map[string]any{
			"mode":             string(cfg.Mode),
			"id":               b.NodeIdentity.Address,
			"listen_addr":      cfg.ListenAddr,
			"node_bridge_addr": cfg.OutboundGateway.NodeBridgeAddr,
			"rv_control_addr":  cfg.OutboundGateway.RVControlAddr,
			"rendezvous_addr":  cfg.OutboundGateway.RendezvousAddr,
			"gate_addr":        cfg.OutboundGateway.GateAddr,
			"auth_mode":        authCfg.Mode,
			"auth_owner":       authCfg.Owner,
			"auth_admins":      authCfg.Admins,
			"auth_users":       authCfg.Users,
			"tls_cert":         cfg.TLS.Cert,
			"tls_key":          cfg.TLS.Key,
		})

	case http.MethodPut:
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}

		b.CfgMu.Lock()
		restartRequired := false

		if v, ok := req["node_bridge_addr"].(string); ok {
			b.Cfg.OutboundGateway.NodeBridgeAddr = v
		}
		if v, ok := req["rv_control_addr"].(string); ok {
			b.Cfg.OutboundGateway.RVControlAddr = v
		}
		if v, ok := req["rendezvous_addr"].(string); ok {
			b.Cfg.OutboundGateway.RendezvousAddr = v
		}
		if v, ok := req["gate_addr"].(string); ok {
			b.Cfg.OutboundGateway.GateAddr = v
		}
		// Auth config updates
		authChanged := false
		if v, ok := req["auth_owner"].(string); ok {
			b.Auth.Owner = v
			authChanged = true
		}
		if v, ok := req["auth_mode"].(string); ok {
			b.Auth.Mode = v
			authChanged = true
		}
		if v, ok := req["auth_admins"].([]interface{}); ok {
			admins := make([]string, 0, len(v))
			for _, a := range v {
				if s, ok := a.(string); ok && s != "" {
					admins = append(admins, s)
				}
			}
			b.Auth.Admins = admins
			authChanged = true
		}
		if v, ok := req["auth_users"].([]interface{}); ok {
			users := make([]string, 0, len(v))
			for _, u := range v {
				if s, ok := u.(string); ok && s != "" {
					users = append(users, s)
				}
			}
			b.Auth.Users = users
			authChanged = true
		}
		if v, ok := req["listen_addr"].(string); ok && v != "" && v != b.Cfg.ListenAddr {
			b.Cfg.ListenAddr = v
			restartRequired = true
		}
		if v, ok := req["tls_cert"].(string); ok && v != b.Cfg.TLS.Cert {
			b.Cfg.TLS.Cert = v
			restartRequired = true
		}
		if v, ok := req["tls_key"].(string); ok && v != b.Cfg.TLS.Key {
			b.Cfg.TLS.Key = v
			restartRequired = true
		}

		cfgCopy := b.Cfg
		authCopy := b.Auth
		b.CfgMu.Unlock()

		if cfgCopy.ConfigFile != "" {
			if err := SaveConfig(cfgCopy); err != nil {
				log.Printf("[admin] config save error: %v", err)
			}
		}
		if authChanged && authCopy.ConfigFile != "" {
			if err := SaveAuthConfig(authCopy); err != nil {
				log.Printf("[admin] auth config save error: %v", err)
			}
		}

		json.NewEncoder(w).Encode(map[string]any{
			"ok":               true,
			"restart_required": restartRequired,
		})

	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// HandleCards returns the per-card UI visibility map. Public — anyone
// rendering the workspace can hide cards the broker owner disabled.
func (b *Base) HandleCards(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodOptions {
		w.WriteHeader(204)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	b.CfgMu.RLock()
	cards := b.Cfg.Cards
	b.CfgMu.RUnlock()
	if cards == nil {
		cards = map[string]CardConfig{}
	}
	json.NewEncoder(w).Encode(map[string]any{"cards": cards})
}

// HandleAdminCards updates the per-card visibility map. Owner only via
// the existing /v1/admin/* middleware. Persists by rewriting broker.json.
func (b *Base) HandleAdminCards(w http.ResponseWriter, r *http.Request) {
	AdminCORS(w)
	if r.Method == http.MethodOptions {
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Cards map[string]CardConfig `json:"cards"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	if req.Cards == nil {
		req.Cards = map[string]CardConfig{}
	}
	b.CfgMu.Lock()
	b.Cfg.Cards = req.Cards
	cfg := b.Cfg
	b.CfgMu.Unlock()
	// Persist to the writable runtime sidecar — broker.json may be on a
	// read-only volume in containers.
	rt := RuntimeOverride{Cards: cfg.Cards, APIFeatures: cfg.APIFeatures}
	if err := SaveRuntime(cfg, rt); err != nil {
		log.Printf("[admin] cards save failed: %v", err)
		http.Error(w, `{"error":"save failed: `+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "cards": req.Cards})
}

// HandleAdminLogs returns recent log lines.
func (b *Base) HandleAdminLogs(w http.ResponseWriter, r *http.Request) {
	AdminCORS(w)
	n := 100
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	lines := b.LogBuf.Recent(n)
	json.NewEncoder(w).Encode(map[string]any{"lines": lines})
}

// logsDir returns the directory where this process writes log files,
// derived from the configured log.file path. Defaults to "logs" relative
// to cwd when no log file is configured.
func (b *Base) logsDir() string {
	if b.Cfg.Log.File == "" {
		return "logs"
	}
	return filepath.Dir(b.Cfg.Log.File)
}

// HandleAdminLogFiles returns the list of log files on disk.
//
// GET /v1/admin/logs/files → [{name, size}]
func (b *Base) HandleAdminLogFiles(w http.ResponseWriter, r *http.Request) {
	AdminCORS(w)
	if r.Method == http.MethodOptions {
		return
	}

	type logFile struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}

	var files []logFile
	dir := b.logsDir()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, logFile{Name: name, Size: info.Size()})
	}

	if files == nil {
		files = []logFile{}
	}
	json.NewEncoder(w).Encode(files)
}

// HandleAdminLogFile returns a tail (last N lines) of a specific log file
// with optional substring search. Path traversal is blocked.
//
// GET /v1/admin/logs/file?name=broker.log&tail=200&q=error
func (b *Base) HandleAdminLogFile(w http.ResponseWriter, r *http.Request) {
	AdminCORS(w)
	if r.Method == http.MethodOptions {
		return
	}

	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
		return
	}
	// Path traversal guard.
	clean := filepath.Clean(name)
	if strings.Contains(clean, "..") || strings.ContainsAny(clean, `/\`) {
		http.Error(w, `{"error":"invalid name"}`, http.StatusBadRequest)
		return
	}

	dir := b.logsDir()
	full := filepath.Join(dir, clean)
	absDir, _ := filepath.Abs(dir)
	absFull, _ := filepath.Abs(full)
	if !strings.HasPrefix(absFull, absDir) {
		http.Error(w, `{"error":"invalid name"}`, http.StatusBadRequest)
		return
	}

	tail := 200
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 2000 {
				n = 2000
			}
			tail = n
		}
	}
	search := strings.ToLower(r.URL.Query().Get("q"))

	f, err := os.Open(full)
	if err != nil {
		http.Error(w, `{"error":"file not found"}`, http.StatusNotFound)
		return
	}
	defer f.Close()

	var all []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	start := len(all) - tail
	if start < 0 {
		start = 0
	}
	lines := all[start:]
	if search != "" {
		filtered := lines[:0]
		for _, l := range lines {
			if strings.Contains(strings.ToLower(l), search) {
				filtered = append(filtered, l)
			}
		}
		lines = filtered
	}
	json.NewEncoder(w).Encode(map[string]any{"lines": lines})
}

// HandleAdminLogsStream sends log lines via SSE.
func (b *Base) HandleAdminLogsStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ch := b.LogBuf.Subscribe()
	defer b.LogBuf.Unsubscribe(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case line := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", strings.TrimRight(line, "\n"))
			flusher.Flush()
		}
	}
}
