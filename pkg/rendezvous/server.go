package rendezvous

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/isannai/isann-servers/pkg/setup"
	"github.com/isannai/isann-servers/pkg/tunnel"
)
// ProxyInfo stores a registered proxy.
type ProxyInfo struct {
	ID           string
	Role         string   // "provider" or "broker"
	Addr         string   // NAT public address (from UDP source IP:Port)
	LocalAddr    string   // LAN address reported by provider (for same-NAT clients)
	CertHash     string
	Version      string
	BinHash      string
	OwnerAddress string
	Emblem       string
	UDPAddr      net.Addr // for sending UDP commands back
	LastSeen     time.Time
	RegisteredAt time.Time

	// AddrManual — when true, Addr was explicitly set by the register
	// payload (dev / static deployment). UDP punch learnPublicAddr won't
	// overwrite. In production NAT setups operators don't know their
	// external addr so msg.Addr is empty, AddrManual stays false, and
	// punch learning fills in Addr live.
	AddrManual bool

	// ControlIP — the source IP of the TCP control connection (hello-
	// bound identity). UDP punch packets are only honored when their
	// source IP matches this — otherwise a third party could forge a
	// punch with another node's ID and redirect the NAT mapping (MITM).
	// Same-host TCP / UDP traffic shares the same external NAT IP in
	// every NAT type we care about (full-cone / restricted / symmetric).
	ControlIP net.IP

	// Node status (updated via status_update messages)
	Status   string              // "idle" | "busy"
	AuthMode string              // "public" | "protected" ("open" = legacy alias)
	Hardware *setup.HardwareSpec // hardware info
	Services []setup.ServiceInfo // running services

	// TPM verification
	TPMVerified    bool             // fTPM challenge passed
	EKFingerprint  string           // SHA256 of EK public key (for dedup)
	EKCertIssuer   string           // EK cert issuer CN (e.g. "CSME ADL PTT 01SVN")
	EKPublicKey    *rsa.PublicKey   // EK public key (for signature verification)
	TPMNonce       []byte           // pending nonce (waiting for response)
}

// TLSConfig holds TLS certificate settings.
type TLSConfig struct {
	Enabled bool   `json:"enabled"`
	Cert    string `json:"cert"`
	Key     string `json:"key"`
}

// Server is the rendezvous server.
type Server struct {
	Addr string // legacy — informational only; no listener uses it anymore
	TLS  TLSConfig

	// UnifiedAddr — the only listener now. Binds both TCP (control plane
	// — long-lived register / heartbeat / server push) and UDP (punch
	// coordination + NAT mapping learning) on the same port number
	// (e.g. ":9100"). Required.
	//
	// Design: separate transports for separate concerns —
	//   TCP = control (reliable, server push, keepalive)
	//   UDP = punch + NAT learning (peer-to-peer data uses peer's own
	//         UDP socket directly, not via RV)
	UnifiedAddr string

	// controlConns — TCP control connection registry, keyed by node_id.
	// Populated when isannd opens its long-lived control connection;
	// removed on disconnect. Used by PushToNode to deliver server-side
	// messages (e.g. "punch start", "remap_request").
	controlConns sync.Map // map[string]*controlConn

	// Optional TCP REST listener. Empty string = off. Protocol is chosen
	// by TLS.Enabled: HTTPS when TLS is on (reuses the same cert/key),
	// plain HTTP otherwise. Serves the same handler as HTTP/3.
	RESTAddr string // e.g. ":8443"

	// ProxyPurge: a proxy entry is purged after now - LastSeen exceeds
	// this value. 0 means "use default" (defaultProxyPurge). Configured
	// via advanced.proxy_purge_sec in config JSON.
	ProxyPurge time.Duration

	// PingIntervals: per-role baseline ping cadence returned to clients
	// on every register_ack. Keys: "provider", "broker". Missing/zero
	// entries fall back to defaultPingInterval. Configured via
	// advanced.ping_interval_sec.{provider,broker} in config JSON.
	PingIntervals map[string]time.Duration

	// RegisterIntervals: per-role baseline register (fullSync re-sync)
	// cadence returned to clients on every register_ack. Same keys and
	// semantics as PingIntervals; default is 300s for both roles.
	// Configured via advanced.register_interval_sec.{provider,broker}.
	RegisterIntervals map[string]time.Duration

	// Mode — admission mode from auth.json: "public" (anyone with a valid
	// register signature may register) or "protected" (a FullSync register
	// must carry a valid issuer-signed admission credential). Empty = public.
	Mode string

	// Issuers — authorized admission issuers for protected mode, keyed by
	// lowercased issuer address → policy (ttl ceiling + bind: node|none|any).
	// Loaded from auth.json next to the config (LoadRVAuth). Empty in public mode.
	Issuers map[string]IssuerPolicy

	// Holepunch — UDP hole-punch parameters RV pushes to requester nodes
	// inside every proxy_info reply. Tuning lives here (one place) instead
	// of in each node's local config. Zero fields fall back to the built-in
	// defaults below.
	Holepunch HolepunchPolicy

	proxies map[string]*ProxyInfo
	mu      sync.RWMutex

	// Phase 1 refine: session cache + per-service metric cache (populated
	// by UDP 0x02 HeartbeatMsg handler, read by /v1/nodes).
	sessions *tunnel.SessionCache
	metricsMu sync.RWMutex
	metrics   map[string]*NodeMetric // key = "nodeID/serviceName"
	heartSeqMu sync.Mutex
	heartSeq   map[string]uint64 // key = "tokenHex/serviceName" → last accepted seq
}

// HolepunchPolicy — RV-controlled UDP hole-punch coordination parameters.
// Shipped to requesters in every proxy_info reply (see handleConnect)
// alongside Candidates. Zero-valued fields fall back to the defaults
// below via Effective*() accessors — that way an operator who only wants
// to tune one knob doesn't have to specify all five.
//
// PrePunchCount / PrePunchIntervalMs / PreDialWaitMs control the
// requester's outbound pre-dial punch round; ReplyBurstCount /
// ReplyBurstIntervalMs control the receiver's burst when a peer punch
// arrives. See pkg/tunnel/rendezvous.go's RendezvousMsg punch fields
// for the over-the-wire encoding.
type HolepunchPolicy struct {
	PrePunchCount        int
	PrePunchIntervalMs   int
	PreDialWaitMs        int
	ReplyBurstCount      int
	ReplyBurstIntervalMs int
}

// Built-in defaults applied when the operator leaves a field at 0.
// Matched against the hardcoded values isannd used before this knob
// was made server-driven (see commit 24958b9 prePunchCandidates,
// d52f10d fireReplyBurst) — flipping these without changing the
// defaults preserves behavior for operators who haven't tuned RV yet.
const (
	defaultPrePunchCount        = 5
	defaultPrePunchIntervalMs   = 50
	defaultPreDialWaitMs        = 100
	defaultReplyBurstCount      = 3
	defaultReplyBurstIntervalMs = 50
)

// EffectivePolicy returns the configured HolepunchPolicy with built-in
// defaults filled in for any zero field. Used by handleConnect when
// writing the proxy_info reply so the wire format always carries
// concrete values (the requester then doesn't have to know defaults).
func (s *Server) EffectivePolicy() HolepunchPolicy {
	p := s.Holepunch
	if p.PrePunchCount <= 0 {
		p.PrePunchCount = defaultPrePunchCount
	}
	if p.PrePunchIntervalMs <= 0 {
		p.PrePunchIntervalMs = defaultPrePunchIntervalMs
	}
	if p.PreDialWaitMs <= 0 {
		p.PreDialWaitMs = defaultPreDialWaitMs
	}
	if p.ReplyBurstCount <= 0 {
		p.ReplyBurstCount = defaultReplyBurstCount
	}
	if p.ReplyBurstIntervalMs <= 0 {
		p.ReplyBurstIntervalMs = defaultReplyBurstIntervalMs
	}
	return p
}

// defaultProxyPurge: how long a proxy entry survives without a ping.
// Tuned for 1Hz pings — 120s = ~120 missed before purge.
const defaultProxyPurge = 120 * time.Second

// ProxyPurgeOrDefault returns the configured purge threshold, or the
// default when unset. Used by the cleanup goroutine and by main() for
// startup logging.
func (s *Server) ProxyPurgeOrDefault() time.Duration {
	if s.ProxyPurge > 0 {
		return s.ProxyPurge
	}
	return defaultProxyPurge
}

// NodeMetric is a snapshot of the latest heartbeat for a (node, service)
// pair. Returned by /v1/nodes merged into per-service fields.
type NodeMetric struct {
	NodeID        string
	ServiceName   string
	Status        int32
	QueueDepth    uint32
	TotalJobsDone uint64
	AvgJobSec     float32
	LastJobMs     int64
	RunningJobID  string
	RunningCount  uint32 // in-flight request count (vLLM num_requests_running)
	// UpdatedAt removed — single source of truth is proxies[NodeID].LastSeen.
	Seq           uint64
}

// defaultPingInterval returns the built-in baseline cadence per role.
// Used when the operator hasn't configured advanced.ping_interval_sec.<role>
// and as the floor when computing stale/offline thresholds.
func defaultPingInterval(role string) time.Duration {
	switch role {
	case "provider", "station":
		return 5 * time.Second
	case "broker", "control":
		return 10 * time.Second
	default:
		return 5 * time.Second
	}
}

// minPingInterval / maxPingInterval clamp operator input so a typo
// can't peg every node to 0.001s or hours.
const (
	minPingInterval = 1 * time.Second
	maxPingInterval = 60 * time.Second
)

// PingIntervalFor returns the configured baseline ping interval for role,
// falling back to defaultPingInterval. Result is clamped to
// [minPingInterval, maxPingInterval].
func (s *Server) PingIntervalFor(role string) time.Duration {
	d := s.PingIntervals[role]
	if d <= 0 {
		d = defaultPingInterval(role)
	}
	if d < minPingInterval {
		d = minPingInterval
	}
	if d > maxPingInterval {
		d = maxPingInterval
	}
	return d
}

// defaultRegisterInterval / min / max — register cadence has no per-role
// distinction (both roles re-sync at the same rate by default); kept as
// constants rather than a switch to highlight that. Min 30s prevents
// thrash, max 1h prevents stale registry under low-churn deployments.
const (
	defaultRegisterInterval = 5 * time.Minute
	minRegisterInterval     = 30 * time.Second
	maxRegisterInterval     = 60 * time.Minute
)

// RegisterIntervalFor returns the configured register cadence for role,
// falling back to defaultRegisterInterval. Result is clamped to
// [minRegisterInterval, maxRegisterInterval].
func (s *Server) RegisterIntervalFor(role string) time.Duration {
	d := s.RegisterIntervals[role]
	if d <= 0 {
		d = defaultRegisterInterval
	}
	if d < minRegisterInterval {
		d = minRegisterInterval
	}
	if d > maxRegisterInterval {
		d = maxRegisterInterval
	}
	return d
}

// connStatusThresholds returns the (stale, offline) thresholds for a role,
// scaled to the baseline interval — 3× baseline for stale, 5× for offline.
// This way the thresholds adjust automatically when the operator changes
// ping cadence in RV config.
func (s *Server) connStatusThresholds(role string) (stale, offline time.Duration) {
	base := s.PingIntervalFor(role)
	return base * 3, base * 5
}

// computeConnStatus 는 LastSeen 값과 role 을 기반으로 연결 상태 문자열을 반환.
// "alive" | "stale" | "offline".
func (s *Server) computeConnStatus(role string, lastSeen, now time.Time) string {
	age := now.Sub(lastSeen)
	stale, offline := s.connStatusThresholds(role)
	if age > offline {
		return "offline"
	}
	if age > stale {
		return "stale"
	}
	return "alive"
}

func NewServer(addr string) *Server {
	return &Server{
		Addr:     addr,
		proxies:  make(map[string]*ProxyInfo),
		sessions: tunnel.NewSessionCache(),
		metrics:  make(map[string]*NodeMetric),
		heartSeq: make(map[string]uint64),
	}
}

func (s *Server) Run() error {
	if s.UnifiedAddr == "" {
		return fmt.Errorf("rendezvous: unified_addr must be set — legacy UDP+HTTP/3 listener was removed")
	}

	// HTTP API handler (shared by REST listener).
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"version": setup.RendezvousVersion,
			"hash":    setup.SelfHash(),
		})
	})
	// /v1/metrics — heartbeat-aggregated per-(node, service) metric rows.
	httpMux.HandleFunc("/v1/metrics", s.handleMetrics)
	httpMux.HandleFunc("/v1/nodes", s.handleNodes)

	// Cleanup stale proxies every 15 seconds + prune sessions and metric cache.
	// Tight tick keeps the time-from-last-ping to UI-disappearance close to
	// the configured cutoff (e.g. cutoff=60s + tick=15s ⇒ ≤75s worst case).
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			cutoff := s.ProxyPurgeOrDefault()
			s.mu.Lock()
			now := time.Now()
			for id, p := range s.proxies {
				if now.Sub(p.LastSeen) > cutoff {
					log.Printf("[rendezvous] cleanup: removed stale proxy %s (last seen %s ago, cutoff=%s)", id, now.Sub(p.LastSeen).Round(time.Second), cutoff)
					delete(s.proxies, id)
				}
			}
			s.mu.Unlock()

			if s.sessions != nil {
				s.sessions.Prune(now)
			}
			s.pruneMetrics(5 * time.Minute)
		}
	}()

	// QUIC signaling on :9001 was removed — provider / broker now ride
	// the NLB TCP control on UnifiedAddr (:9100) via isannd.

	// REST listener (HTTPS when TLS, plain HTTP otherwise). Optional.
	if s.RESTAddr != "" {
		go func() {
			if s.TLS.Enabled && s.TLS.Cert != "" && s.TLS.Key != "" {
				log.Printf("[rendezvous] REST (HTTPS/TCP) on %s", s.RESTAddr)
				if err := http.ListenAndServeTLS(s.RESTAddr, s.TLS.Cert, s.TLS.Key, httpMux); err != nil {
					log.Printf("[rendezvous] REST HTTPS listener exited: %v", err)
				}
				return
			}
			log.Printf("[rendezvous] REST (HTTP/TCP) on %s", s.RESTAddr)
			if err := http.ListenAndServe(s.RESTAddr, httpMux); err != nil {
				log.Printf("[rendezvous] REST HTTP listener exited: %v", err)
			}
		}()
	}

	// Phase 3 unified port — TCP control + UDP punch on the same port
	// number (e.g. :9100). UDP punch runs as a goroutine; TCP control
	// blocks Run() so the process exits cleanly when the listener stops.
	go func() {
		if err := s.runPunchUDP(context.Background(), s.UnifiedAddr); err != nil {
			log.Printf("[rendezvous] punch UDP error: %v", err)
		}
	}()

	log.Printf("Rendezvous server (unified=%q, rest=%q)", s.UnifiedAddr, s.RESTAddr)
	return s.runControlTCP(context.Background(), s.UnifiedAddr)
}

// signal.Handler implementation removed — the QUIC signaling listener
// on :9001 was deleted. Register / Connect / TPM challenge now flow
// over the TCP control plane in control_tcp.go's handleControlConn.

// handleNodes returns registered nodes with status info.
// Query params (all optional):
//
//	q       — filter by node ID substring
//	online  — "true" to return only online nodes
//	service — filter by service name
//	status  — filter by node status ("idle" | "busy")
//	page    — page number (1-based); requires limit
//	limit   — page size; if omitted with page, defaults to 50
//
// Without page+limit the full filtered list is returned (array).
// With page+limit returns {"nodes":[...],"total":N,"page":P,"limit":L}.
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-ISANN-Message, If-None-Match, Content-Type")
	w.Header().Set("Access-Control-Expose-Headers", "ETag")
	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	q := r.URL.Query()
	// node_id is an exact lookup — when present, every other filter is
	// ignored. Comma-separated to request multiple specific nodes at once.
	var nodeIDSet map[string]bool
	if raw := q.Get("node_id"); raw != "" {
		nodeIDSet = make(map[string]bool)
		for _, id := range strings.Split(raw, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				nodeIDSet[id] = true
			}
		}
	}

	search := strings.ToLower(q.Get("q"))
	onlineOnly := q.Get("online") == "true"
	svcFilter := strings.ToLower(q.Get("service"))
	statusFilter := strings.ToLower(q.Get("status"))
	roleFilter := strings.ToLower(q.Get("role"))
	modelFilter := q.Get("model")
	gpuFilter := strings.ToLower(q.Get("gpu"))
	minVRAM := 0.0
	if v := q.Get("min_vram"); v != "" {
		if p, err := strconv.ParseFloat(v, 64); err == nil {
			minVRAM = p
		}
	}
	pageStr := q.Get("page")
	limitStr := q.Get("limit")
	paginate := pageStr != "" || limitStr != ""

	// status filter is a metrics-side concept — pre-compute node_id set from
	// the metric cache so we can filter static-side rows by cross-ref.
	// When a service filter is also active, narrow the check to metrics
	// for that specific (node, service) pair so `service=llm-api&status=idle`
	// means "llm-api is idle" rather than "any service is idle."
	var statusMatchNodes map[string]bool
	if statusFilter != "" {
		statusMatchNodes = make(map[string]bool)
		s.metricsMu.RLock()
		for _, m := range s.metrics {
			if heartbeatStatus(m.Status) != statusFilter {
				continue
			}
			if svcFilter != "" && strings.ToLower(m.ServiceName) != svcFilter {
				continue
			}
			statusMatchNodes[m.NodeID] = true
		}
		s.metricsMu.RUnlock()
	}

	page, limit := 1, 50
	if v, err := strconv.Atoi(pageStr); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(limitStr); err == nil && v > 0 {
		limit = v
	}

	// /v1/nodes is the static endpoint. Live fields (online, conn_status,
	// status, last_seen) moved to /v1/metrics so this response is stable
	// for ETag/cache purposes — only changes on register events.
	type node struct {
		ID           string              `json:"id"`
		Role         string              `json:"role,omitempty"`
		Addr         string              `json:"addr"`
		StartedAt    string              `json:"started_at,omitempty"`
		ConnectedAt  string              `json:"connected_at,omitempty"`
		Version      string              `json:"version,omitempty"`
		BinHash      string              `json:"bin_hash,omitempty"`
		OwnerAddress string              `json:"owner_address,omitempty"`
		Emblem       string              `json:"emblem,omitempty"`
		AuthMode     string              `json:"auth_mode,omitempty"`
		Hardware     *setup.HardwareSpec `json:"hardware,omitempty"`
		Services     []setup.ServiceInfo `json:"services,omitempty"`
		TPMVerified  bool                `json:"tpm_verified,omitempty"`
		EKCertIssuer string              `json:"ek_cert_issuer,omitempty"`
	}

	now := time.Now()
	s.mu.RLock()
	var all []node
	var cntProvider, cntBroker, cntConsumer int
	for _, p := range s.proxies {
		online := now.Sub(p.LastSeen) < 90*time.Second

		// Role tally over (online-filtered) registrations — counts ALL roles
		// including consumers, which are then skipped from the directory list
		// below. This is the ONLY way the Gate can aggregate consumer counts:
		// they never appear in the node list. Independent of display filters.
		if !onlineOnly || online {
			switch strings.ToLower(p.Role) {
			case "provider", "station":
				cntProvider++
			case "broker", "control":
				cntBroker++
			case "consumer", "passenger":
				cntConsumer++
			}
		}

		// Passenger nodes (role=passenger, "P:" id; legacy "consumer"/"C:")
		// register with RV ONLY so hole-punch can reach them (RV learns their
		// UDP mapping for the return-punch). They serve nothing and are never a
		// dial target, so they must never surface in the node directory /
		// discovery — skip unconditionally, even for an explicit node_id lookup.
		if strings.EqualFold(p.Role, "consumer") || strings.EqualFold(p.Role, "passenger") {
			continue
		}

		// node_id lookup — bypass every other filter.
		if nodeIDSet != nil {
			if !nodeIDSet[p.ID] {
				continue
			}
		} else {
			if onlineOnly && !online {
				continue
			}
			if roleFilter != "" && strings.ToLower(p.Role) != roleFilter {
				continue
			}
			// status is metrics-side — cross-referenced via statusMatchNodes built above.
			if statusMatchNodes != nil && !statusMatchNodes[p.ID] {
				continue
			}
			if search != "" && !strings.Contains(strings.ToLower(p.ID), search) && !strings.Contains(strings.ToLower(p.OwnerAddress), search) {
				continue
			}
			if svcFilter != "" || modelFilter != "" {
				found := false
				for _, svc := range p.Services {
					if svcFilter != "" && strings.ToLower(svc.Name) != svcFilter {
						continue
					}
					if modelFilter != "" && svc.Model != modelFilter {
						continue
					}
					found = true
					break
				}
				if !found {
					continue
				}
			}
			if gpuFilter != "" && !hasGPUName(p.Hardware, gpuFilter) {
				continue
			}
			if minVRAM > 0 && maxGPUVRAM(p.Hardware) < minVRAM {
				continue
			}
		}

		// /v1/nodes is static-only now. Per-service volatile (queue_depth,
		// total_jobs_done, avg_job_sec, status) is served by /v1/metrics.
		// We emit Services as-is (engine/model/version/ready) and strip any
		// stale volatile fields that an old provider may still include.
		staticSvcs := make([]setup.ServiceInfo, len(p.Services))
		for i, svc := range p.Services {
			staticSvcs[i] = setup.ServiceInfo{
				Name:           svc.Name,
				Type:           svc.Type,
				Launcher:       svc.Launcher,
				Version:        svc.Version,
				BinHash:        svc.BinHash,
				Model:          svc.Model,
				ModelHash:      svc.ModelHash,
				ModelOriginURL: svc.ModelOriginURL,
				ServerReady:    svc.ServerReady,
				ServerLoading: svc.ServerLoading,
				ChildPID:      svc.ChildPID,
				ChildName:     svc.ChildName,
				MaxQueue:      svc.MaxQueue,
				Concurrency:   svc.Concurrency,
				QueueDisabled: svc.QueueDisabled,
				Inspect:       svc.Inspect,
				InspectLabels: svc.InspectLabels,
				InspectOrder:  svc.InspectOrder,
			}
		}

		n := node{
			ID:           p.ID,
			Role:         p.Role,
			Addr:         p.Addr,
			Version:      p.Version,
			BinHash:      p.BinHash,
			OwnerAddress: p.OwnerAddress,
			Emblem:       p.Emblem,
			AuthMode:     p.AuthMode,
			Hardware:     setup.StableHardware(p.Hardware),
			Services:     staticSvcs,
			TPMVerified:  p.TPMVerified,
			EKCertIssuer: p.EKCertIssuer,
		}
		if !p.RegisteredAt.IsZero() {
			n.StartedAt = p.RegisteredAt.UTC().Format(time.RFC3339)
		}
		// connected_at — when the node's CURRENT TCP control conn was
		// established (resets on reconnect, unlike started_at which survives
		// brief drops within the purge window). A stable timestamp, not a live
		// duration, so /v1/nodes stays ETag-stable; the client computes elapsed
		// time. From the live controlConns registry (p.ID is nodeKey-normalized,
		// matching the controlConns key set in handleControlConn).
		if ccVal, ok := s.controlConns.Load(p.ID); ok {
			if cc, ok := ccVal.(*controlConn); ok && !cc.connectedAt.IsZero() {
				n.ConnectedAt = cc.connectedAt.UTC().Format(time.RFC3339)
			}
		}
		all = append(all, n)
		_ = online // sorting moved to client (live data lives in /v1/metrics)
	}
	s.mu.RUnlock()

	// Sort by ID — online sort moved to client (online comes from /v1/metrics).
	sort.Slice(all, func(i, j int) bool {
		return all[i].ID < all[j].ID
	})

	if all == nil {
		all = []node{}
	}

	var body []byte
	if !paginate {
		body, _ = json.Marshal(all)
	} else {
		total := len(all)
		start := (page - 1) * limit
		if start > total {
			start = total
		}
		end := start + limit
		if end > total {
			end = total
		}
		body, _ = json.Marshal(map[string]any{
			"nodes": all[start:end],
			"total": total,
			"page":  page,
			"limit": limit,
		})
	}

	// ETag hash now stable — derived/live fields (online, conn_status,
	// status, last_seen) moved to /v1/metrics. /v1/nodes only changes on
	// actual register events, so 304 is hit reliably.
	type etagNode struct {
		ID           string              `json:"id"`
		Role         string              `json:"role,omitempty"`
		Addr         string              `json:"addr"`
		AuthMode     string              `json:"auth_mode,omitempty"`
		Hardware     *setup.HardwareSpec `json:"hardware,omitempty"`
		Services     []setup.ServiceInfo `json:"services,omitempty"`
	}
	etagNodes := make([]etagNode, len(all))
	for i, n := range all {
		etagNodes[i] = etagNode{ID: n.ID, Role: n.Role, Addr: n.Addr, AuthMode: n.AuthMode, Hardware: n.Hardware, Services: n.Services}
	}
	etagBody, _ := json.Marshal(etagNodes)
	// Fold the consumer count into the ETag: consumers are excluded from the
	// node list (and thus etagNodes), but their join/leave IS a real change a
	// caching client (the Gate) must see, so it has to invalidate. Provider /
	// broker changes already move etagNodes.
	etagBody = append(etagBody, []byte(fmt.Sprintf("|c=%d", cntConsumer))...)
	h := sha256.Sum256(etagBody)
	etag := `"` + hex.EncodeToString(h[:8]) + `"`
	// Per-role counts (incl. consumers, which the list hides) for the Gate to
	// aggregate. Set before the 304 check so it rides every response.
	w.Header().Set("X-Role-Counts", fmt.Sprintf("provider=%d,broker=%d,consumer=%d", cntProvider, cntBroker, cntConsumer))
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(200)
	w.Write(body)
}

// handleMetrics returns heartbeat-aggregated service metrics as flat rows
// (one per node+service combination). Filters cross-reference static info
// from the proxy registry so model/gpu queries work even though those
// fields are not in the heartbeat payload itself.
//
// Query params (all optional; no params = every row):
//
//	service   — exact service name (e.g. "llm-api")
//	model     — exact model name (static cross-ref)
//	gpu       — substring match on GPU name (static cross-ref)
//	status    — "idle" | "busy" | "loading" | "stopped"
//	src       — short-hand for service (returns only rows for that service)
//	min_vram  — minimum VRAM GB on any GPU (static cross-ref)
//
// Response: [{node_id, service, status, queue_depth, total_jobs_done, avg_job_sec}]
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-ISANN-Message, Content-Type")
	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	q := r.URL.Query()

	// node_id lookup — when present, all other filters are ignored.
	// Comma-separated to request multiple nodes at once.
	var nodeIDSet map[string]bool
	if raw := q.Get("node_id"); raw != "" {
		nodeIDSet = make(map[string]bool)
		for _, id := range strings.Split(raw, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				nodeIDSet[id] = true
			}
		}
	}

	fService := q.Get("service")
	if src := q.Get("src"); src != "" && fService == "" {
		fService = src
	}
	fModel := q.Get("model")
	fGPU := strings.ToLower(q.Get("gpu"))
	fStatus := strings.ToLower(q.Get("status"))
	fMinVRAM := 0.0
	if v := q.Get("min_vram"); v != "" {
		if p, err := strconv.ParseFloat(v, 64); err == nil {
			fMinVRAM = p
		}
	}

	// Build allowed node_id set when any static-cross-ref filter is active.
	needStaticFilter := fModel != "" || fGPU != "" || fMinVRAM > 0
	allowed := make(map[string]bool)
	if needStaticFilter {
		s.mu.RLock()
		for id, p := range s.proxies {
			if p.Role != "" && p.Role != "provider" && p.Role != "station" {
				continue
			}
			if fGPU != "" && !hasGPUName(p.Hardware, fGPU) {
				continue
			}
			if fMinVRAM > 0 && maxGPUVRAM(p.Hardware) < fMinVRAM {
				continue
			}
			if fModel != "" {
				match := false
				for _, svc := range p.Services {
					if svc.Model == fModel {
						if fService == "" || svc.Name == fService {
							match = true
							break
						}
					}
				}
				if !match {
					continue
				}
			}
			allowed[id] = true
		}
		s.mu.RUnlock()
	}

	type row struct {
		NodeID        string  `json:"node_id"`
		Service       string  `json:"service"`
		Status        string  `json:"status"`
		QueueDepth    uint32  `json:"queue_depth"`
		RunningCount  uint32  `json:"running_count"`
		TotalJobsDone uint64  `json:"total_jobs_done"`
		AvgJobSec     float32 `json:"avg_job_sec,omitempty"`
		LastJobMs     int64   `json:"last_job_ms,omitempty"`
		RunningJobID  string  `json:"running_job_id,omitempty"`
		UpdatedAt     int64   `json:"updated_at"`  // = proxies[node].LastSeen (unix ms)
		ConnStatus    string  `json:"conn_status"` // alive | stale | offline
		Online        bool    `json:"online"`      // last_seen within 60s
	}

	// Snapshot per-node liveness (LastSeen + Role) under proxy lock first,
	// then iterate metrics under its own lock — never hold both at once.
	type liveness struct {
		lastSeen time.Time
		role     string
	}
	live := make(map[string]liveness)
	s.mu.RLock()
	for id, p := range s.proxies {
		live[id] = liveness{lastSeen: p.LastSeen, role: p.Role}
	}
	s.mu.RUnlock()

	now := time.Now()
	out := []row{}
	seenNodes := map[string]bool{}
	s.metricsMu.RLock()
	for _, m := range s.metrics {
		// node_id lookup wins — ignore other filters when present.
		if nodeIDSet != nil {
			if !nodeIDSet[m.NodeID] {
				continue
			}
		} else {
			if needStaticFilter && !allowed[m.NodeID] {
				continue
			}
			if fService != "" && m.ServiceName != fService {
				continue
			}
		}
		statusStr := heartbeatStatus(m.Status)
		if nodeIDSet == nil && fStatus != "" && statusStr != fStatus {
			continue
		}
		l := live[m.NodeID]
		seenNodes[m.NodeID] = true
		out = append(out, row{
			NodeID:        m.NodeID,
			Service:       m.ServiceName,
			Status:        statusStr,
			QueueDepth:    m.QueueDepth,
			RunningCount:  m.RunningCount,
			TotalJobsDone: m.TotalJobsDone,
			AvgJobSec:     m.AvgJobSec,
			LastJobMs:     m.LastJobMs,
			RunningJobID:  m.RunningJobID,
			UpdatedAt:     l.lastSeen.UnixMilli(),
			ConnStatus:    s.computeConnStatus(l.role, l.lastSeen, now),
			Online:        !l.lastSeen.IsZero() && now.Sub(l.lastSeen) < 60*time.Second,
		})
	}
	s.metricsMu.RUnlock()

	// Emit synthetic node-level rows for proxies that registered but have no
	// service metrics yet (e.g. freshly registered, or all services disabled).
	// Skip when fService filter is set — caller wants only specific service rows.
	if fService == "" && fStatus == "" {
		for nid, l := range live {
			if seenNodes[nid] {
				continue
			}
			if nodeIDSet != nil {
				if !nodeIDSet[nid] {
					continue
				}
			} else if needStaticFilter && !allowed[nid] {
				continue
			}
			out = append(out, row{
				NodeID:     nid,
				Service:    "",
				UpdatedAt:  l.lastSeen.UnixMilli(),
				ConnStatus: s.computeConnStatus(l.role, l.lastSeen, now),
				Online:     !l.lastSeen.IsZero() && now.Sub(l.lastSeen) < 60*time.Second,
			})
		}
	}

	_ = json.NewEncoder(w).Encode(out)
}


// hasGPUName reports whether any GPU's name contains needle (case-insensitive).
func hasGPUName(hw *setup.HardwareSpec, needle string) bool {
	if hw == nil {
		return false
	}
	for _, g := range hw.GPUs {
		if strings.Contains(strings.ToLower(g.Name), needle) {
			return true
		}
	}
	return false
}

// maxGPUVRAM returns the largest VRAM among installed GPUs (0 if none).
func maxGPUVRAM(hw *setup.HardwareSpec) float64 {
	if hw == nil {
		return 0
	}
	var max float64
	for _, g := range hw.GPUs {
		if g.VramTotalGB > max {
			max = g.VramTotalGB
		}
	}
	return max
}
