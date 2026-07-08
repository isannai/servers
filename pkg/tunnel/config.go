package tunnel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/isannai/isann-servers/pkg/glog"
	"github.com/isannai/isann-servers/pkg/setup"
)

// IannConfig is the root anchor file (`<root>/isann.config.json`). A backend
// binary (station / control) finds it by walking up from its own location, then
// uses it to derive every other directory (conf, packages, models, outputs, logs).
//
// Minimal form: `{"version": "1.0"}`. All Paths fields are optional —
// missing means "use default relative to root".
type IannConfig struct {
	Version    string         `json:"version"`
	InstanceID string         `json:"instance_id,omitempty"`
	Paths      IannConfigPath `json:"paths,omitempty"`
}

type IannConfigPath struct {
	Conf     string `json:"conf,omitempty"`
	Packages string `json:"packages,omitempty"`
	Models   string `json:"models,omitempty"`
	Outputs  string `json:"outputs,omitempty"`
	Logs     string `json:"logs,omitempty"`
}

// FindRoot walks up from startDir looking for `isann.config.json`. Returns
// the absolute root directory + parsed config. Stops at filesystem root.
//
// Caller is expected to pass the backend binary's directory (`os.Executable()` →
// `filepath.Dir`) so the binary can be relocated and still find its anchor.
func FindRoot(startDir string) (string, *IannConfig, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", nil, err
	}
	for i := 0; i < 12; i++ {
		path := filepath.Join(dir, "isann.config.json")
		if data, rerr := os.ReadFile(path); rerr == nil {
			var cfg IannConfig
			if jerr := json.Unmarshal(data, &cfg); jerr != nil {
				return "", nil, fmt.Errorf("parse isann.config.json: %w", jerr)
			}
			return dir, &cfg, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", nil, fmt.Errorf("isann.config.json not found (searched up from %s)", startDir)
}

// ResolvePath joins root with override (or fallback default) when override is
// relative. Absolute overrides win as-is. Empty override falls back to def.
func ResolvePath(root, override, def string) string {
	p := override
	if p == "" {
		p = def
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}

// Mode defines how the proxy operates.
type Mode string

const (
	ModeControl Mode = "control" // control-center role (console + /node forward; was "broker"/"client")
	ModeStation Mode = "station" // inference-serving role (was "provider"/"server")

	// Backward compatibility aliases
	ModeClient Mode = "client"
	ModeServer Mode = "server"
)

// NormalizeMode converts legacy mode names to current names: "client"/"broker" →
// control, "server"/"provider" → station.
func NormalizeMode(m Mode) Mode {
	switch m {
	case "client", "broker":
		return ModeControl
	case "server", "provider":
		return ModeStation
	default:
		return m
	}
}

// DBConfig holds database driver and connection settings.
type DBConfig struct {
	Driver string `json:"driver,omitempty"`
	DSN    string `json:"dsn,omitempty"`
}

// QueueConfig is the Provider-wide queue settings — output_dir for results,
// retention TTL for orphan cleanup at startup. Per-service overrides live in
// `Services[].Queue` (setup.QueueOverride); engine defaults live in manifest's
// `queue.default_*` fields. See docs/TODO/queue-provider-migration.md.
type QueueConfig struct {
	OutputDir    string `json:"output_dir,omitempty"`     // result directory; "" = memory-only
	OutputTTLSec int    `json:"output_ttl_sec,omitempty"` // orphan cleanup cutoff at boot; 0 = skip cleanup
}

// TimeoutConfig holds broker-side timeouts for QUIC tunnel calls into provider
// nodes. All values are in seconds; 0 (or omitted) means use the built-in
// default. Only effective when Mode == "broker".
//
//   - Provider:    response wait for a generic provider call (svc, installer,
//                  control endpoints). Hard upper bound — frontends usually
//                  time out much sooner. Default 180.
//   - Connect:     initial QUIC dial budget (rendezvous + hole punching).
//                  Default 20.
//   - StaleDetect: attempt-1 readDeadline used to detect stale cached QUIC
//                  connections after provider restart. Short value (5s) lets
//                  the broker invalidate the dead conn and retry within the
//                  frontend's own fetch timeout. Auto-skipped on ?wait=true
//                  sync calls where slow first byte is expected. Default 5.
type TimeoutConfig struct {
	ProviderSec    int `json:"provider,omitempty"`
	ConnectSec     int `json:"connect,omitempty"`
	StaleDetectSec int `json:"stale_detect,omitempty"`
}

// Provider returns the response timeout for generic provider calls.
func (t TimeoutConfig) Provider() time.Duration {
	if t.ProviderSec <= 0 {
		return 3 * time.Minute
	}
	return time.Duration(t.ProviderSec) * time.Second
}

// Connect returns the initial QUIC dial timeout.
func (t TimeoutConfig) Connect() time.Duration {
	if t.ConnectSec <= 0 {
		return 20 * time.Second
	}
	return time.Duration(t.ConnectSec) * time.Second
}

// StaleDetect returns the attempt-1 readDeadline for stale-conn detection.
// Negative values disable the optimization (1st attempt uses Provider()).
func (t TimeoutConfig) StaleDetect() time.Duration {
	if t.StaleDetectSec == 0 {
		return 5 * time.Second
	}
	if t.StaleDetectSec < 0 {
		return 0
	}
	return time.Duration(t.StaleDetectSec) * time.Second
}

// TLSConfig holds TLS certificate settings.
type TLSConfig struct {
	Enabled bool   `json:"enabled"`
	Cert    string `json:"cert"`
	Key     string `json:"key"`
}

// OutboundGatewayConfig — backend (broker / provider) container 가 외부세상
// (RV, Gate, peer) 으로 나갈 때 거치는 단일 게이트웨이. 새 architecture 에서는
// isannd sidecar 가 이 역할을 함. backend 는 외부에 직접 접근하지 않고 isannd 한테
// 위임하므로, 이 블록의 두 줄이면 충분.
type OutboundGatewayConfig struct {
	// NodeBridgeAddr — backend 가 isannd 의 node-bridge HTTP listener
	// 한테 보낼 base URL (예: "http://127.0.0.1:8443"). 비어있으면
	// default "http://127.0.0.1:8443". HTTP-style endpoints (TPM
	// measurement, /internal/rv/* GETs, /internal/gate/* proxy,
	// /node/<id>/*) 이리로 감.
	NodeBridgeAddr string `json:"node_bridge_addr,omitempty"`

	// RVControlAddr — isannd 의 rv-control TCP listener (NLB byte-bridge)
	// host:port. 비어있으면 default "127.0.0.1:19100". backend 가 여기로
	// long-lived TCP 소켓 열고 register / heartbeat / service_event 를
	// signal.WriteFrame 으로 직접 송신. 응답 (need_register / ack) 와
	// server-push 도 같은 소켓으로 받음.
	RVControlAddr string `json:"rv_control_addr,omitempty"`

	// Legacy fields — direct-dial RV from broker/provider was removed
	// when QUIC signaling on :9001 went away. Kept on the struct so old
	// configs continue to parse (json unmarshal silently ignores them).
	// New configs should leave them empty; admin/status display reads
	// only the NodeBridge / RVControl fields above.
	RendezvousAddr string `json:"rendezvous_addr,omitempty"`
	GateAddr       string `json:"gate_addr,omitempty"`
}

// URL returns the base URL for the outbound gateway (isannd's node-bridge
// HTTP listener). Format:
//   - operator set NodeBridgeAddr with scheme: returned verbatim
//   - operator set NodeBridgeAddr without scheme: http:// prepended
//   - NodeBridgeAddr empty + RVControlAddr set: derive host from
//     RVControlAddr, use port 8443 (the standard node-bridge listener port)
//   - both empty: http://127.0.0.1:8443
//
// Special case: if NodeBridgeAddr explicitly points at the NLB TCP port
// (19100), the host portion is reused with port 8443 — operators who
// collapsed their config to a single field don't accidentally end up
// with the HTTP client hitting the raw-TCP listener.
func (g OutboundGatewayConfig) URL() string {
	if g.NodeBridgeAddr != "" {
		raw := g.NodeBridgeAddr
		raw = strings.TrimPrefix(raw, "https://")
		raw = strings.TrimPrefix(raw, "http://")
		// raw = "host:port" or "host"
		host := raw
		port := ""
		if idx := strings.LastIndex(raw, ":"); idx >= 0 {
			host = raw[:idx]
			port = raw[idx+1:]
		}
		if host == "" {
			host = "127.0.0.1"
		}
		// If the operator pointed at the NLB TCP port, swap to the
		// node-bridge HTTP port so HTTP calls don't hit the wrong
		// listener.
		if port == "19100" {
			port = "8443"
		}
		if port == "" {
			port = "8443"
		}
		return "http://" + host + ":" + port
	}
	// NodeBridgeAddr empty — derive host from RVControlAddr when set.
	if g.RVControlAddr != "" {
		raw := g.RVControlAddr
		raw = strings.TrimPrefix(raw, "tcp://")
		host := raw
		if idx := strings.LastIndex(raw, ":"); idx >= 0 {
			host = raw[:idx]
		}
		if host == "" {
			host = "127.0.0.1"
		}
		return "http://" + host + ":8443"
	}
	return "http://127.0.0.1:8443"
}

// ControlHostPort returns the host:port of isannd's rv-control TCP listener
// (NLB byte-bridge). Defaults to "127.0.0.1:19100" when RVControlAddr is
// empty. When the operator set RVControlAddr we strip any tcp:// scheme.
//
// When the operator left RVControlAddr empty but did set NodeBridgeAddr
// (the HTTP base), the host portion of NodeBridgeAddr is reused with the
// standard NLB port 19100 — so an operator who runs both listeners on
// the same isannd host only needs to spell out one field. Defaults to
// 127.0.0.1:19100 when nothing is set anywhere.
func (g OutboundGatewayConfig) ControlHostPort() string {
	if g.RVControlAddr != "" {
		addr := g.RVControlAddr
		addr = strings.TrimPrefix(addr, "tcp://")
		return addr
	}
	// Derive from NodeBridgeAddr's host when only that is set.
	if g.NodeBridgeAddr != "" {
		raw := g.NodeBridgeAddr
		raw = strings.TrimPrefix(raw, "https://")
		raw = strings.TrimPrefix(raw, "http://")
		// raw is now "host:port" or "host". Extract host.
		host := raw
		if idx := strings.LastIndex(raw, ":"); idx >= 0 {
			host = raw[:idx]
		}
		if host == "" {
			host = "127.0.0.1"
		}
		return host + ":19100"
	}
	return "127.0.0.1:19100"
}

// RendezvousHostPort returns the host:port portion of the legacy RendezvousAddr,
// stripping any scheme. Used by deadcode UDP heartbeat / direct dial paths;
// pending removal.
func (g OutboundGatewayConfig) RendezvousHostPort() string {
	addr := g.RendezvousAddr
	addr = strings.TrimPrefix(addr, "https://")
	addr = strings.TrimPrefix(addr, "http://")
	return addr
}

// AuthConfig holds authentication settings (auth.json).
type AuthConfig struct {
	Mode           string   `json:"mode"`            // "public" or "protected" ("open" = legacy alias for "public")
	Owner          string   `json:"owner"`
	Issuer         string   `json:"issuer,omitempty"`
	Admins         []string `json:"admins,omitempty"`
	Users          []string `json:"users,omitempty"`
	RevokedGrants  []string `json:"revoked_grants,omitempty"`
	ConfigFile     string   `json:"-"`
}

// LoadAuthConfig reads auth.json from the same directory as the main config file.
func LoadAuthConfig(mainConfigPath string) (AuthConfig, error) {
	var ac AuthConfig
	dir := filepath.Dir(mainConfigPath)
	path := filepath.Join(dir, "auth.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			ac.Mode = "public"
			ac.ConfigFile = path
			return ac, nil
		}
		return ac, fmt.Errorf("read auth config: %w", err)
	}
	if err := json.Unmarshal(data, &ac); err != nil {
		return ac, fmt.Errorf("parse auth config: %w", err)
	}
	ac.ConfigFile = path
	if ac.Mode == "open" {
		ac.Mode = "public" // legacy alias → canonical "public"
	}
	return ac, nil
}

// SaveAuthConfig writes auth config to its file.
func SaveAuthConfig(ac AuthConfig) error {
	if ac.ConfigFile == "" {
		return fmt.Errorf("no auth config file path set")
	}
	if err := os.MkdirAll(filepath.Dir(ac.ConfigFile), 0755); err != nil {
		return fmt.Errorf("create auth config dir: %w", err)
	}
	data, err := json.MarshalIndent(ac, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal auth config: %w", err)
	}
	return os.WriteFile(ac.ConfigFile, data, 0644)
}

// IsPublic returns true if auth mode is public — no signature required for
// user-level access. "open" is accepted as a legacy alias for "public".
func (ac *AuthConfig) IsPublic() bool {
	return ac.Mode == "public" || ac.Mode == "open" || ac.Mode == ""
}

// CardConfig is a per-card UI visibility entry.
type CardConfig struct {
	Enabled bool `json:"enabled"`
}

// TTLsConfig removed — ping/register cadences are now RV-driven via
// register_ack (see pkg/rendezvous + pkg/tunnel/rendezvous.go's
// PingIntervalSec / RegisterIntervalSec fields). Node-side fallback
// constants live in pkg/control/rendezvous.go and pkg/station/rendezvous.go
// for the cold-start / RV-down case.

// Config holds proxy configuration.
type Config struct {
	Mode               Mode                 `json:"mode"`
	ListenAddr         string               `json:"listen_addr"`
	OutboundGateway    OutboundGatewayConfig `json:"outbound_gateway"`
	TLS                TLSConfig            `json:"tls"`
	Queue              QueueConfig          `json:"queue,omitempty"`
	Timeout            TimeoutConfig        `json:"timeout,omitempty"`
	Services           []setup.ServiceEntry `json:"services"`
	InstallerAddr      string               `json:"installer_addr"`
	DB                 DBConfig             `json:"db"`
	WebDir             string               `json:"web_dir"`
	ExposeHardwareInfo bool                 `json:"expose_hardware_info"`
	HomeDir            string               `json:"home_dir,omitempty"`
	Emblem             string               `json:"emblem,omitempty"`
	// ExternalAddr — operator-declared external dial target for peer
	// HTTP/3. Used when the node is behind a NAT with port forwarding
	// configured: provider sets this to "<public-ip>:<isannd-port>",
	// register includes msg.Addr=<this>, RV stores AddrManual=true, and
	// peers dial that addr directly. Bypasses NAT hole punching.
	// Empty = let RV learn the punch-socket NAT mapping (production NAT
	// hole punching path).
	ExternalAddr string `json:"external_addr,omitempty"`
	Log                glog.Config          `json:"log"`
	// Cards is the per-card UI visibility map. Key = card id (e.g. "logs",
	// "settings"). Missing key = default enabled (frontend decides). Owner
	// edits via PUT /v1/admin/cards; everyone reads via GET /v1/cards.
	Cards map[string]CardConfig `json:"cards,omitempty"`
	// APIFeatures is the per-feature backend gating map. Key = feature name
	// (see pkg/control/apipolicy.AllFeatures). Missing key = preset default
	// (central). Owner edits via PUT /v1/admin/api-features; clients read
	// via GET /v1/api/policy.
	APIFeatures map[string]CardConfig `json:"api_features,omitempty"`
	ConfigFile  string                `json:"-"`
}

// LoadConfig reads a JSON config file and returns a Config.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	cfg.ConfigFile = path
	cfg.Mode = NormalizeMode(cfg.Mode)

	// Overlay runtime overrides (cards / api_features) from a writable
	// sidecar file. Lets the immutable conf stay on a read-only volume in
	// containerized deployments.
	if rt, rerr := LoadRuntime(cfg); rerr == nil {
		ApplyRuntime(&cfg, rt)
	}
	return cfg, nil
}

// SaveConfig writes the current config to its ConfigFile path.
func SaveConfig(cfg Config) error {
	if cfg.ConfigFile == "" {
		return fmt.Errorf("no config file path set")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.ConfigFile), 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(cfg.ConfigFile, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
