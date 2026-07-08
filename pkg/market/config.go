package market

// config.go — market backend configuration. Mirrors pkg/gate/config.go:
// a JSON file with an addr, TLS block, DB driver/DSN, and an optional web
// dir for the isann-registry SPA. Unlike gate, market has NO auth.json —
// write access is per-asset (author = signer), verified per request from the
// canonical signature (§4 market-server.md), not from an operator allow-list.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/isannai/isann-servers/pkg/glog"
)

// TLSConfig holds TLS certificate settings.
type TLSConfig struct {
	Enabled bool   `json:"enabled"`
	Cert    string `json:"cert"`
	Key     string `json:"key"`
}

// DBConfig holds database driver and connection settings.
type DBConfig struct {
	Driver string `json:"driver,omitempty"` // "sqlite" (dev, default) | "mysql" (prod)
	DSN    string `json:"dsn,omitempty"`
}

// Config holds market server configuration. market is a pure JSON API server —
// there is no web_dir: the public site (isann-registry) is a separate app that
// consumes these endpoints over CORS-open public read.
type Config struct {
	Addr       string      `json:"addr"`
	TLS        TLSConfig   `json:"tls"`
	DB         DBConfig    `json:"db"`
	ReplayWinS int         `json:"replay_window_sec"` // write signature timestamp tolerance (default 300)
	Log        glog.Config `json:"log"`

	// DevInsecureSkipAuth disables ALL write/private-read signature checks
	// (signature, nonce, timestamp) — writes become plain curl. LOCAL DEV ONLY.
	// Never set in production: it removes the provenance/replay guarantees.
	DevInsecureSkipAuth bool `json:"dev_insecure_skip_auth,omitempty"`
}

// LoadConfig reads a JSON config file and returns a Config, filling defaults.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

// applyDefaults fills the zero-value fields so a minimal (or empty) config
// still yields a runnable server — handy for tests and first-boot.
func (cfg *Config) applyDefaults() {
	if cfg.Addr == "" {
		cfg.Addr = ":8820"
	}
	if cfg.DB.Driver == "" {
		cfg.DB.Driver = "sqlite"
	}
	if cfg.DB.DSN == "" {
		cfg.DB.DSN = "market.db"
	}
	if cfg.ReplayWinS <= 0 {
		cfg.ReplayWinS = 300
	}
	// Auto-create the DB file's parent dir (sqlite only — mysql DSN has no path).
	if cfg.DB.Driver == "sqlite" {
		if dir := filepath.Dir(cfg.DB.DSN); dir != "." && dir != "" {
			os.MkdirAll(dir, 0755)
		}
	}
}
