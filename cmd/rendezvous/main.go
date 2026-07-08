package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"

	"github.com/isannai/isann-servers/pkg/certgen"
	"github.com/isannai/isann-servers/pkg/glog"
	"github.com/isannai/isann-servers/pkg/rendezvous"
)

type tlsConfig struct {
	Enabled bool   `json:"enabled"`
	Cert    string `json:"cert"`
	Key     string `json:"key"`
}

// pingIntervalConfig — per-role baseline ping cadence (seconds) RV dictates to
// clients in each register_ack. Keyed by the NEW role names (station/control);
// the old names (provider/broker) are accepted as legacy aliases so pre-rename
// configs keep working. Use station()/control() to read (new wins over legacy).
type pingIntervalConfig struct {
	Station int `json:"station,omitempty"` // station baseline ping (s). 0 → default (5s)
	Control int `json:"control,omitempty"` // control baseline ping (s). 0 → default (10s)
	// legacy aliases (provider→station, broker→control).
	Provider int `json:"provider,omitempty"`
	Broker   int `json:"broker,omitempty"`
}

func (p pingIntervalConfig) station() int { return firstNonZero(p.Station, p.Provider) }
func (p pingIntervalConfig) control() int { return firstNonZero(p.Control, p.Broker) }

// registerIntervalConfig — per-role baseline register (fullSync re-sync) cadence
// (seconds). Same station/control + legacy provider/broker scheme as ping.
type registerIntervalConfig struct {
	Station int `json:"station,omitempty"`
	Control int `json:"control,omitempty"`
	// legacy aliases.
	Provider int `json:"provider,omitempty"`
	Broker   int `json:"broker,omitempty"`
}

func (r registerIntervalConfig) station() int { return firstNonZero(r.Station, r.Provider) }
func (r registerIntervalConfig) control() int { return firstNonZero(r.Control, r.Broker) }

// firstNonZero returns the first non-zero value (new name wins over legacy).
func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// holepunchConfig — JSON-facing form of rendezvous.HolepunchPolicy.
// Operator-tunable knobs RV ships to requesters via register_ack.
// Zero fields fall back to rendezvous package defaults (see
// EffectivePolicy in pkg/rendezvous/server.go).
type holepunchConfig struct {
	PrePunchCount        int `json:"pre_punch_count,omitempty"`
	PrePunchIntervalMs   int `json:"pre_punch_interval_ms,omitempty"`
	PreDialWaitMs        int `json:"pre_dial_wait_ms,omitempty"`
	ReplyBurstCount      int `json:"reply_burst_count,omitempty"`
	ReplyBurstIntervalMs int `json:"reply_burst_interval_ms,omitempty"`
}

type advancedConfig struct {
	// ProxyPurgeSec is how long a node entry survives without a ping (sec).
	// Once now - LastSeen exceeds this, the entry is removed from the
	// in-memory registry. 0 (or unset) → default 120s.
	ProxyPurgeSec int `json:"proxy_purge_sec,omitempty"`
	// PingInterval — per-role baseline ping cadence returned to clients
	// on each register_ack.
	PingInterval pingIntervalConfig `json:"ping_interval_sec,omitempty"`
	// RegisterInterval — per-role baseline register (fullSync re-sync)
	// cadence returned to clients on each register_ack.
	RegisterInterval registerIntervalConfig `json:"register_interval_sec,omitempty"`
	// Holepunch — UDP hole-punch coordination parameters RV pushes to
	// requesters in every register_ack.
	Holepunch holepunchConfig `json:"holepunch,omitempty"`
}

type config struct {
	Addr string    `json:"addr,omitempty"` // legacy — ignored; kept so old configs parse without error
	TLS  tlsConfig `json:"tls"`
	// UnifiedAddr — Phase 3 unified port (e.g. ":9100"). Binds both TCP
	// (control plane) and UDP (punch coordination + NAT mapping). Operator
	// opens one firewall rule for the pair. REQUIRED — RV will refuse to
	// start with empty UnifiedAddr now that the legacy UDP+HTTP/3 and
	// QUIC signaling listeners have been removed.
	UnifiedAddr string `json:"unified_addr,omitempty"`
	// Optional TCP REST API listener (e.g. ":9000"). HTTPS when tls.enabled,
	// plain HTTP otherwise. Empty = off.
	RESTAddr string         `json:"rest_addr,omitempty"`
	Advanced advancedConfig `json:"advanced,omitempty"`
	Log      glog.Config    `json:"log"`
}

func main() {
	configPath := flag.String("config", "", "config file path (JSON)")
	flag.Parse()

	var cfg config

	if *configPath != "" {
		data, err := os.ReadFile(*configPath)
		if err != nil {
			log.Fatalf("read config: %v", err)
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			log.Fatalf("parse config: %v", err)
		}
	}

	if cfg.UnifiedAddr == "" {
		cfg.UnifiedAddr = ":9100"
	}

	// Logging is hardcoded — conf's `log` block intentionally ignored.
	// Canonical: logs/rendezvous.log relative to CWD. Daily rotation,
	// 14 files retained. Matches the broker/gate/provider pattern.
	// "both" so the local terminal also sees lines during dev — file
	// remains the authoritative archive.
	glog.New(glog.Config{
		Output:   "both",
		File:     "logs/rendezvous.log",
		Rotate:   "daily",
		MaxFiles: 14,
	})

	// Auto-generate a self-signed cert on first boot when TLS is on but none is
	// provided, so `docker compose up` works out of the box. Production mounts
	// real certs (no-op when they already exist).
	if cfg.TLS.Enabled {
		if err := certgen.EnsureSelfSigned(cfg.TLS.Cert, cfg.TLS.Key); err != nil {
			log.Fatalf("certgen: %v", err)
		}
	}

	srv := rendezvous.NewServer(cfg.Addr)
	srv.TLS = rendezvous.TLSConfig{
		Enabled: cfg.TLS.Enabled,
		Cert:    cfg.TLS.Cert,
		Key:     cfg.TLS.Key,
	}
	srv.UnifiedAddr = cfg.UnifiedAddr
	srv.RESTAddr = cfg.RESTAddr
	// Admission mode + issuer allowlist from auth.json (next to the config).
	// Missing file = public mode. protected mode requires issuer-signed
	// admission credentials on register.
	mode, issuers, err := rendezvous.LoadRVAuth(*configPath)
	if err != nil {
		log.Fatalf("load auth.json: %v", err)
	}
	srv.Mode = mode
	srv.Issuers = issuers
	if cfg.Advanced.ProxyPurgeSec > 0 {
		srv.ProxyPurge = time.Duration(cfg.Advanced.ProxyPurgeSec) * time.Second
	}
	// Apply each interval to BOTH the new and legacy role names — the wire still
	// carries provider/broker (node rename is Phase 3), so the server must key
	// both until the whole rename lands.
	srv.PingIntervals = map[string]time.Duration{}
	if v := cfg.Advanced.PingInterval.station(); v > 0 {
		d := time.Duration(v) * time.Second
		srv.PingIntervals["station"] = d
		srv.PingIntervals["provider"] = d
	}
	if v := cfg.Advanced.PingInterval.control(); v > 0 {
		d := time.Duration(v) * time.Second
		srv.PingIntervals["control"] = d
		srv.PingIntervals["broker"] = d
	}
	srv.RegisterIntervals = map[string]time.Duration{}
	if v := cfg.Advanced.RegisterInterval.station(); v > 0 {
		d := time.Duration(v) * time.Second
		srv.RegisterIntervals["station"] = d
		srv.RegisterIntervals["provider"] = d
	}
	if v := cfg.Advanced.RegisterInterval.control(); v > 0 {
		d := time.Duration(v) * time.Second
		srv.RegisterIntervals["control"] = d
		srv.RegisterIntervals["broker"] = d
	}
	srv.Holepunch = rendezvous.HolepunchPolicy{
		PrePunchCount:        cfg.Advanced.Holepunch.PrePunchCount,
		PrePunchIntervalMs:   cfg.Advanced.Holepunch.PrePunchIntervalMs,
		PreDialWaitMs:        cfg.Advanced.Holepunch.PreDialWaitMs,
		ReplyBurstCount:      cfg.Advanced.Holepunch.ReplyBurstCount,
		ReplyBurstIntervalMs: cfg.Advanced.Holepunch.ReplyBurstIntervalMs,
	}
	effPolicy := srv.EffectivePolicy()
	log.Printf("Starting rendezvous server (unified=%q rest=%q mode=%q issuers=%d proxy_purge=%s ping={provider:%s broker:%s} register={provider:%s broker:%s} holepunch={pre_punch=%dx%dms wait=%dms reply_burst=%dx%dms})",
		cfg.UnifiedAddr, cfg.RESTAddr, srv.Mode, len(srv.Issuers),
		srv.ProxyPurgeOrDefault(),
		srv.PingIntervalFor("provider"), srv.PingIntervalFor("broker"),
		srv.RegisterIntervalFor("provider"), srv.RegisterIntervalFor("broker"),
		effPolicy.PrePunchCount, effPolicy.PrePunchIntervalMs, effPolicy.PreDialWaitMs,
		effPolicy.ReplyBurstCount, effPolicy.ReplyBurstIntervalMs)
	if err := srv.Run(); err != nil {
		log.Fatal(err)
	}
}
