package tunnel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RuntimeOverride is the writable subset of broker config — values that
// the owner toggles at runtime via the Settings UI. Persisted next to the
// log file (which is necessarily writable) so the immutable broker.json
// can stay on a read-only volume in containerized deployments.
type RuntimeOverride struct {
	Cards       map[string]CardConfig `json:"cards,omitempty"`
	APIFeatures map[string]CardConfig `json:"api_features,omitempty"`
}

// runtimePath chooses the location for the runtime override file.
//
// Priority:
//  1. BROKER_RUNTIME_PATH env (explicit override — recommended in containers)
//  2. Same dir as ConfigFile (alongside broker.json)
//
// In container deployments where /app/conf/ is mounted read-only, set
// BROKER_RUNTIME_PATH to a writable location, e.g. /app/state/runtime.json,
// and mount that path as a writable volume.
func runtimePath(cfg Config) string {
	if v := os.Getenv("BROKER_RUNTIME_PATH"); v != "" {
		return v
	}
	if cfg.ConfigFile != "" {
		return filepath.Join(filepath.Dir(cfg.ConfigFile), "broker-runtime.json")
	}
	return "broker-runtime.json"
}

// LoadRuntime reads the runtime override file. Missing file is not an error.
func LoadRuntime(cfg Config) (RuntimeOverride, error) {
	path := runtimePath(cfg)
	var r RuntimeOverride
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return r, fmt.Errorf("read runtime: %w", err)
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("parse runtime: %w", err)
	}
	return r, nil
}

// SaveRuntime writes the runtime override file (atomic-ish).
func SaveRuntime(cfg Config, r RuntimeOverride) error {
	path := runtimePath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal runtime: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write runtime: %w", err)
	}
	return nil
}

// ApplyRuntime overlays runtime override values on top of a Config in place.
// Called once after LoadConfig so cards / api_features come from runtime
// even when broker.json on a read-only volume has stale or missing values.
func ApplyRuntime(cfg *Config, r RuntimeOverride) {
	if r.Cards != nil {
		cfg.Cards = r.Cards
	}
	if r.APIFeatures != nil {
		cfg.APIFeatures = r.APIFeatures
	}
}
