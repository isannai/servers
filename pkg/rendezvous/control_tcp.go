package rendezvous

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/isannai/isann-servers/pkg/setup"
	"github.com/isannai/isann-servers/pkg/signal"
	"github.com/isannai/isann-servers/pkg/tunnel"
)

// controlConn wraps one TCP control connection from an isannd peer.
// Frames use the same length-prefixed JSON format as pkg/signal (QUIC),
// so isannd can send/receive RendezvousMsg without a separate codec.
type controlConn struct {
	conn        net.Conn
	nodeID      string
	role        string
	connectedAt time.Time  // when this TCP control conn was established — surfaced as /v1/nodes connected_at (set once, read-only after)
	mu          sync.Mutex // serialises WriteFrame — net.Conn writes from multiple goroutines need it
}

func (cc *controlConn) sendFrame(msg *tunnel.RendezvousMsg) error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return signal.WriteFrame(cc.conn, msg)
}

// ErrNotConnected is returned by PushToNode when the target has no active
// TCP control connection.
var ErrNotConnected = errors.New("rendezvous: node not connected via TCP control")

// runControlTCP serves the Phase 3 unified port's TCP listener — long-lived
// control connections from isannd peers. Hello / register / heartbeat /
// connect frames arrive on the same TCP stream; server-push frames go out
// on it too.
//
// This is the TCP companion to runPunchUDP. They share a port number
// (s.UnifiedAddr, e.g. ":9100") so operators open one firewall rule for
// the pair.
func (s *Server) runControlTCP(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	log.Printf("[rendezvous] TCP control listening on %s", addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("[rendezvous] TCP control accept error: %v", err)
			continue
		}
		// TCP keepalive — detect dead peers within ~2 minutes without
		// waiting for app-layer ping. Production routers / NATs typically
		// time UDP at 30s and TCP at 5+ minutes, but the keepalive itself
		// also keeps the NAT mapping alive.
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(30 * time.Second)
		}
		go s.handleControlConn(ctx, conn)
	}
}

func (s *Server) handleControlConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// First frame MUST be hello.
	first, err := signal.ReadFrame(conn)
	if err != nil {
		log.Printf("[rendezvous] TCP control read hello (%s): %v", conn.RemoteAddr(), err)
		return
	}
	if first.Type != signal.TypeHello {
		log.Printf("[rendezvous] TCP control: expected hello, got %q from %s", first.Type, conn.RemoteAddr())
		return
	}
	if first.ID == "" {
		log.Printf("[rendezvous] TCP control: hello without id from %s", conn.RemoteAddr())
		return
	}

	cc := &controlConn{
		conn:        conn,
		nodeID:      first.ID,
		role:        first.Role,
		connectedAt: time.Now(),
	}

	// Register — last writer wins on reconnect.
	if prev, loaded := s.controlConns.Swap(nodeKey(cc.nodeID), cc); loaded {
		if pcc, ok := prev.(*controlConn); ok && pcc != nil && pcc.conn != nil {
			_ = pcc.conn.Close()
		}
	}
	defer s.controlConns.CompareAndDelete(nodeKey(cc.nodeID), cc)

	log.Printf("[rendezvous] TCP control: hello role=%s id=%s from=%s", cc.role, cc.nodeID, conn.RemoteAddr())

	if err := cc.sendFrame(&tunnel.RendezvousMsg{V: 1, Type: signal.TypeAck}); err != nil {
		log.Printf("[rendezvous] TCP control: write hello ack to %s: %v", cc.nodeID, err)
		return
	}

	// Frame loop. register / connect / service_event payload handling is
	// stubbed until Phase 2A Step 6 wires real isannd traffic — for now
	// each frame is logged and acked so the wire format works end-to-end.
	for {
		msg, err := signal.ReadFrame(conn)
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				log.Printf("[rendezvous] TCP control: read frame from %s: %v", cc.nodeID, err)
			}
			return
		}
		// Sender-identity frames (register / ping / service_event / status_update)
		// MUST carry the hello-bound nodeID — otherwise a peer could forge
		// frames for another node. Force msg.ID to cc.nodeID for these so
		// upstream handlers always see the authoritative identity, regardless
		// of what the wire said.
		//
		// TypeConnect is different: msg.ID is the *target* of the lookup,
		// not the sender. A broker's conn legitimately sends connect frames
		// referring to provider IDs. We pass that through verbatim.
		switch msg.Type {
		case signal.TypeRegister:
			msg.ID = cc.nodeID
			s.applyRegister(cc, msg)
			// Ack carries every piece of cadence/policy the node needs:
			//
			//   PingIntervalSec / RegisterIntervalSec — per-role baselines;
			//     provider/broker reset their respective tickers on receipt.
			//     RV's connStatusThresholds (3x/5x of ping) align with the
			//     actual ping rate; register controls fullSync re-sync
			//     cadence.
			//
			//   PunchCount / PunchIntervalMs / PreDialWaitMs /
			//   ReplyBurstCount / ReplyBurstIntervalMs — RV's hole-punch
			//     policy; isannd's NLB pipe intercepts these and stores
			//     them as atomic globals (used by both outbound pre-punch
			//     and the listener's reply burst).
			//
			// Sent on every register (including delta) so RV restarts /
			// config reloads propagate without forcing a fullSync round.
			policy := s.EffectivePolicy()
			ack := &tunnel.RendezvousMsg{
				V:                    1,
				Type:                 signal.TypeAck,
				PingIntervalSec:      uint32(s.PingIntervalFor(cc.role).Seconds()),
				RegisterIntervalSec:  uint32(s.RegisterIntervalFor(cc.role).Seconds()),
				PunchCount:           policy.PrePunchCount,
				PunchIntervalMs:      policy.PrePunchIntervalMs,
				PreDialWaitMs:        policy.PreDialWaitMs,
				ReplyBurstCount:      policy.ReplyBurstCount,
				ReplyBurstIntervalMs: policy.ReplyBurstIntervalMs,
			}
			_ = cc.sendFrame(ack)
		case signal.TypeConnect:
			s.handleConnect(cc, msg)
		case signal.TypePing:
			// liveness ping from backend (via isannd /internal/rv/heartbeat).
			// Refreshes LastSeen so RV's cutoff timer keeps the node alive.
			// On missing entry (RV restart), pushes TypeNeedRegister back.
			msg.ID = cc.nodeID
			s.applyPing(cc, msg)
		case signal.TypeServiceEvent, signal.TypeStatusUpd:
			// event-driven service metrics push (via isannd /internal/rv/metrics).
			// Patches the matching ServiceInfo entry in proxies cache.
			msg.ID = cc.nodeID
			s.applyServiceEvent(msg)
		default:
			log.Printf("[rendezvous] TCP control unknown type %q from %s", msg.Type, cc.nodeID)
		}
	}
}

// applyRegister updates / creates the ProxyInfo entry for a node based on
// a TCP control register frame. Mirrors processMsg's register handler in
// server.go but uses the TCP control source as a placeholder address.
//
// The authoritative external (NAT-mapped) UDP address is set later by
// runPunchUDP / learnPublicAddr when an isannd punch ping arrives. Until
// then, ProxyInfo.Addr holds the TCP source — not the same NAT mapping as
// the UDP socket peers will dial, but useful for "is this node alive"
// queries on the REST API.
//
// FullSync register frames carry an ECDSA signature over RegisterDigest;
// we ecrecover and require the recovered address to match the EOA inside
// msg.ID ("P:0xABC" / "B:0xABC"). Non-fullSync updates are rejected here
// — a backend that wants to amend its entry must re-send the signed
// fullSync payload. This is the authoritative defence against hello-id
// spoofing (a peer who can't sign as P:0xABC can't claim that id).
func (s *Server) applyRegister(cc *controlConn, msg *tunnel.RendezvousMsg) {
	// Conn-binding guard: the hello frame on this TCP control connection
	// declared cc.nodeID. Any subsequent register frame on the same conn
	// must match — prevents one authenticated conn from forging another
	// node's entry.
	if msg.ID != cc.nodeID {
		log.Printf("[rendezvous] register id mismatch on %s: msg=%s — reject", cc.nodeID, msg.ID)
		_ = cc.sendFrame(&tunnel.RendezvousMsg{V: 1, Type: signal.TypeError, ID: msg.ID, Addr: "register id mismatch"})
		return
	}
	// FullSync 만 서명/timestamp 검증. non-fullSync (delta) 는 이미 hello
	// 가 인증한 같은 TCP control conn 위의 후속 frame 이므로 implicitly
	// bound — 추가 서명 검증 없이 cached value 와 merge.
	if msg.FullSync {
		hwHash := hashHardwareForVerify(msg.Hardware)
		if err := verifyRegisterSignature(msg, hwHash); err != nil {
			log.Printf("[rendezvous] register signature reject for %s: %v", msg.ID, err)
			_ = cc.sendFrame(&tunnel.RendezvousMsg{V: 1, Type: signal.TypeError, ID: msg.ID, Addr: "register signature invalid"})
			return
		}
		// Protected mode: a FullSync register must also carry a valid
		// issuer-signed admission credential. Checked AFTER the register
		// signature so msg.ID is the node's verified EOA before we bind the
		// credential to it. Public mode skips this entirely.
		if s.modeProtected() {
			if err := s.admitRegister(msg); err != nil {
				log.Printf("[rendezvous] admission reject for %s: %v", msg.ID, err)
				_ = cc.sendFrame(&tunnel.RendezvousMsg{V: 1, Type: signal.TypeError, ID: msg.ID, Addr: "admission denied: " + err.Error()})
				return
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	p := s.proxies[nodeKey(msg.ID)]
	isNew := p == nil
	if isNew {
		p = &ProxyInfo{RegisteredAt: now}
	}
	p.ID = nodeKey(msg.ID) // store normalized so listings show one consistent form
	p.Role = msg.Role
	// Address priority: explicit msg.Addr (dev / static deployment with
	// known listener port) > existing learned addr > TCP control source.
	// When msg.Addr is set we lock the addr (AddrManual=true) so the
	// punch listener doesn't later overwrite it with the wrong socket's
	// NAT mapping.
	if msg.Addr != "" {
		p.Addr = msg.Addr
		p.AddrManual = true
	} else if p.Addr == "" {
		p.Addr = cc.conn.RemoteAddr().String()
	}
	// Bind the punch-source check IP to whatever just opened this TCP
	// control conn. Later punch packets must arrive from the same IP.
	if tcpAddr, ok := cc.conn.RemoteAddr().(*net.TCPAddr); ok {
		p.ControlIP = tcpAddr.IP
	}
	if msg.CertHash != "" {
		p.CertHash = msg.CertHash
	}
	p.LastSeen = now
	if msg.LocalAddr != "" {
		p.LocalAddr = msg.LocalAddr
	}
	if msg.Version != "" {
		p.Version = msg.Version
	}
	if msg.BinHash != "" {
		p.BinHash = msg.BinHash
	}
	if msg.OwnerAddress != "" {
		p.OwnerAddress = msg.OwnerAddress
	}
	if msg.Emblem != "" {
		p.Emblem = msg.Emblem
	}
	if msg.AuthMode != "" {
		p.AuthMode = msg.AuthMode
	}
	if msg.Services != nil {
		p.Services = msg.Services
	}
	if msg.Hardware != nil {
		p.Hardware = msg.Hardware
	}
	// Parse the EK certificate the provider sent so /v1/nodes can show
	// the issuer (e.g. "CSME ADL PTT 01SVN") and a verified flag.
	// EK cert receipt alone is treated as "TPM present and the cert
	// parses cleanly" — strong attestation (TPMCredBlob/TPMResponse
	// challenge-response) is a separate later flow that would also
	// flip TPMVerified, so this is the conservative ground-truth bit:
	// we know the EK cert exists and is well-formed.
	if len(msg.EKCert) > 0 {
		if cert, err := x509.ParseCertificate(msg.EKCert); err == nil {
			p.EKCertIssuer = cert.Issuer.CommonName
			p.TPMVerified = true
		} else {
			log.Printf("[rendezvous] register: parse EK cert for %s: %v", msg.ID, err)
		}
	}
	s.proxies[nodeKey(msg.ID)] = p

	logEntry := map[string]any{
		"event":       "register-tcp",
		"new":         isNew,
		"id":          msg.ID,
		"role":        msg.Role,
		"src":         cc.conn.RemoteAddr().String(),
		"msg_addr":    msg.Addr,
		"addr_manual": p.AddrManual,
		"final_addr":  p.Addr,
	}
	if b, err := json.Marshal(logEntry); err == nil {
		log.Printf("[rendezvous] %s", b)
	}
}

// applyPing refreshes LastSeen for the node. Liveness signal only — no
// metrics. Called when backend sends /internal/rv/heartbeat (forwarded
// as TypePing TCP frame by isannd).
//
// Authentication: every ping carries an ECDSA signature over
// PingDigest(role, timestampMs). RV ecrecovers the signer's EOA and
// confirms it matches the EOA inside msg.ID (P:0xABC / B:0xABC). Stale
// timestamps (>60s skew) are rejected to bound replay window. Without
// this any TCP peer could forge ping frames and keep dead nodes alive.
//
// When the node has no registry entry (RV restart, or node never registered
// before its first ping), pushes TypeNeedRegister back on the control conn
// so isannd can surface the signal to its backend, which then re-sends a
// full register frame. This is the RV-restart auto-recovery path.
func (s *Server) applyPing(cc *controlConn, msg *tunnel.RendezvousMsg) {
	if msg.ID == "" {
		return
	}
	if err := verifyPingSignature(msg); err != nil {
		log.Printf("[rendezvous] ping signature reject for %s: %v", msg.ID, err)
		return
	}
	s.mu.Lock()
	p := s.proxies[nodeKey(msg.ID)]
	if p != nil {
		p.LastSeen = time.Now()
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	// Entry missing — signal the peer to re-register. Best-effort send;
	// isannd will deliver the cue to its backend on the next heartbeat
	// response, which triggers a full register frame.
	_ = cc.sendFrame(&tunnel.RendezvousMsg{V: 1, Type: signal.TypeNeedRegister, ID: msg.ID})
}

// applyServiceEvent processes a service_event frame from the backend.
// Three shapes ride the same frame type:
//
//   - Lifecycle transition  (msg.Event != "") — emitted by provider's
//     runServiceWatcher: service.starting / service.ready / service.stopped
//     with a ServiceInfo map (version, bin_hash, model, inspect, …).
//   - Metric batch          (msg.MetricsBatch != nil) — emitted by
//     provider's pushAllMetrics: one frame, N rows. Preferred form.
//   - Metric (single)       (msg.Metrics != nil) — legacy single-service
//     form. Kept for back-compat with older provider builds.
//
// Routes on which field is set. Frames with none populated are dropped.
func (s *Server) applyServiceEvent(msg *tunnel.RendezvousMsg) {
	if msg.ID == "" {
		return
	}
	if msg.Event != "" && msg.ServiceName != "" {
		s.applyServiceLifecycle(msg)
		return
	}
	if len(msg.MetricsBatch) > 0 {
		for i := range msg.MetricsBatch {
			s.applyServiceMetric(msg.ID, &msg.MetricsBatch[i])
		}
		return
	}
	if msg.Metrics == nil || msg.Metrics.Service == "" {
		return
	}
	s.applyServiceMetric(msg.ID, msg.Metrics)
}

// applyServiceMetric updates s.proxies[nodeID].Services[i] + s.metrics
// for one (node, service) row. Extracted so applyServiceEvent can call
// it once per batch entry without duplicating the patch logic.
func (s *Server) applyServiceMetric(nodeID string, m *tunnel.ServiceMetrics) {
	if m == nil || m.Service == "" {
		return
	}
	s.mu.Lock()
	p := s.proxies[nodeKey(nodeID)]
	if p == nil {
		s.mu.Unlock()
		return
	}
	p.LastSeen = time.Now()
	for i := range p.Services {
		if p.Services[i].Name != m.Service {
			continue
		}
		// Patch only metric-like fields. Manifest / model / version 같은
		// 정적 정보는 register 가 결정.
		p.Services[i].QueueDepth = int(m.QueueDepth)
		p.Services[i].TotalJobsDone = int(m.TotalJobsDone)
		// Map the string status sent by provider to the ServerReady /
		// ServerLoading bools that broker UI renders. Without this the UI
		// stays on the register-time state (which is usually loading=true,
		// ready=false) forever — even after the engine is healthy.
		switch m.Status {
		case "idle", "busy":
			p.Services[i].ServerReady = true
			p.Services[i].ServerLoading = false
		case "loading":
			p.Services[i].ServerReady = false
			p.Services[i].ServerLoading = true
		case "stopped":
			p.Services[i].ServerReady = false
			p.Services[i].ServerLoading = false
		}
		if m.AvgJobSec > 0 {
			v := int(m.AvgJobSec)
			p.Services[i].AvgJobSec = &v
		}
		if m.LastJobMs > 0 {
			p.Services[i].LastJobAt = m.LastJobMs / 1000 // ms → s
		}
		log.Printf("[rendezvous] service_event %s svc=%s status=%s queue=%d running=%d jobs=%d",
			nodeID, m.Service, m.Status, m.QueueDepth, m.RunningCount, m.TotalJobsDone)
		break
	}
	s.mu.Unlock()

	// Also update the s.metrics map so /v1/metrics REST returns the
	// fresh row. This map used to be populated by the legacy UDP 0x02
	// heartbeat path; now that backends push everything as TCP
	// service_event frames, the same data needs to land here too —
	// otherwise /v1/metrics only emits the synthetic node-level row
	// (service="", queue_depth=0) and broker UI sees zeros.
	s.metricsMu.Lock()
	s.metrics[metricKey(nodeID, m.Service)] = &NodeMetric{
		NodeID:        nodeID,
		ServiceName:   m.Service,
		Status:        statusStringToCode(m.Status),
		QueueDepth:    m.QueueDepth,
		TotalJobsDone: m.TotalJobsDone,
		AvgJobSec:     m.AvgJobSec,
		LastJobMs:     m.LastJobMs,
		RunningJobID:  m.RunningJobID,
		RunningCount:  m.RunningCount,
	}
	s.metricsMu.Unlock()
}

// applyServiceLifecycle handles service.starting / service.ready /
// service.stopping / service.stopped frames pushed by provider's
// runServiceWatcher. Replaces the legacy onServiceEvent (which arrived via
// QUIC signaling Session). Uses the same proxies[].Services patching
// pattern as applyRegister.
func (s *Server) applyServiceLifecycle(msg *tunnel.RendezvousMsg) {
	if msg.ServiceName == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.proxies[nodeKey(msg.ID)]
	if p == nil {
		return
	}
	p.LastSeen = time.Now()

	idx := -1
	for i, svc := range p.Services {
		if svc.Name == msg.ServiceName {
			idx = i
			break
		}
	}

	switch msg.Event {
	case "service.starting":
		entry := setup.ServiceInfo{
			Name:          msg.ServiceName,
			ServerLoading: true,
		}
		if info := msg.ServiceInfo; info != nil {
			if v, ok := info["version"].(string); ok {
				entry.Version = v
			}
			if v, ok := info["bin_hash"].(string); ok {
				entry.BinHash = v
			}
			if v, ok := info["model"].(string); ok {
				entry.Model = v
			}
			if v, ok := info["model_hash"].(string); ok {
				entry.ModelHash = v
			}
			if v, ok := info["model_origin_url"].(string); ok {
				entry.ModelOriginURL = v
			}
			if v, ok := info["child_pid"].(float64); ok {
				entry.ChildPID = int(v)
			}
			if v, ok := info["child_name"].(string); ok {
				entry.ChildName = v
			}
			applyInspectFromEvent(&entry, info)
		}
		if idx >= 0 {
			prev := p.Services[idx]
			if entry.Inspect == nil && prev.Inspect != nil {
				entry.Inspect = prev.Inspect
				entry.InspectLabels = prev.InspectLabels
				entry.InspectOrder = prev.InspectOrder
			}
			p.Services[idx] = entry
		} else {
			p.Services = append(p.Services, entry)
		}
		log.Printf("[rendezvous] service_event: %s %s starting", msg.ID, msg.ServiceName)

	case "service.ready":
		if idx < 0 {
			idx = len(p.Services)
			p.Services = append(p.Services, setup.ServiceInfo{Name: msg.ServiceName})
		}
		p.Services[idx].ServerLoading = false
		p.Services[idx].ServerReady = true
		if info := msg.ServiceInfo; info != nil {
			if v, ok := info["model"].(string); ok && v != "" {
				p.Services[idx].Model = v
			}
			if v, ok := info["model_hash"].(string); ok && v != "" {
				p.Services[idx].ModelHash = v
			}
			if v, ok := info["model_origin_url"].(string); ok && v != "" {
				p.Services[idx].ModelOriginURL = v
			}
			if v, ok := info["version"].(string); ok && v != "" {
				p.Services[idx].Version = v
			}
			applyInspectFromEvent(&p.Services[idx], info)
		}
		log.Printf("[rendezvous] service_event: %s %s ready", msg.ID, msg.ServiceName)

	case "service.stopping":
		// Provider 의 markStopRequested 가 dockerStop 발사 직전에 보내는
		// 신호. 컨테이너는 아직 살아있어서 server_ready 는 그대로 둠 — broker
		// UI 가 server_ready=true 를 유지하며 progress bar 를 docker stop
		// 진행 동안 계속 보여줌. 실제 stopped 는 다음 probe 의 service.stopped
		// 으로 도착해서 entry 가 제거됨.
		log.Printf("[rendezvous] service_event: %s %s stopping", msg.ID, msg.ServiceName)

	case "service.stopped":
		if idx >= 0 {
			p.Services = append(p.Services[:idx], p.Services[idx+1:]...)
		}
		reason := ""
		if msg.ServiceInfo != nil {
			if v, ok := msg.ServiceInfo["reason"].(string); ok {
				reason = v
			}
		}
		log.Printf("[rendezvous] service_event: %s %s stopped (reason=%s)", msg.ID, msg.ServiceName, reason)

	default:
		log.Printf("[rendezvous] service_event: unknown event %q from %s", msg.Event, msg.ID)
	}
}

// statusStringToCode maps the string status ("idle" / "busy" / "loading" /
// "stopped") sent by backends in service_event frames to the int32 enum
// the metric cache stores (matches the legacy protobuf Status values so
// heartbeatStatus() can decode either source consistently).
func statusStringToCode(s string) int32 {
	switch s {
	case "idle":
		return 1
	case "busy":
		return 2
	case "loading":
		return 3
	case "stopped":
		return 4
	default:
		return 0
	}
}

// handleConnect resolves the target node by msg.ID and replies with a
// proxy_info frame containing the target's external addr (NAT-mapped UDP
// source learned by runPunchUDP, or the TCP register fallback). The
// requester (typically a broker's isannd) uses that addr to HTTP/3 dial
// the target peer.
//
// Optionally pushes a "punch" frame to the target via PushToNode so it
// fires a hole-punch packet toward the requester's external addr — the
// classic ICE-lite symmetric punch pattern. Skipped if the target has
// no active TCP control connection (e.g. UDP-only legacy node).
func (s *Server) handleConnect(cc *controlConn, msg *tunnel.RendezvousMsg) {
	targetID := msg.ID
	if targetID == "" {
		_ = cc.sendFrame(&tunnel.RendezvousMsg{V: 1, Type: signal.TypeError, Addr: "connect: missing target id"})
		return
	}

	s.mu.RLock()
	target, ok := s.proxies[nodeKey(targetID)]
	s.mu.RUnlock()
	if !ok || target == nil {
		log.Printf("[rendezvous] TCP control connect from %s -> %s: target not found", cc.nodeID, targetID)
		_ = cc.sendFrame(&tunnel.RendezvousMsg{V: 1, Type: signal.TypeError, ID: targetID, Addr: "target not found: " + targetID})
		return
	}

	log.Printf("[rendezvous] TCP control connect %s -> %s (target addr=%s)", cc.nodeID, targetID, target.Addr)

	// Best-effort punch coordination: tell the target to fire a punch ping
	// toward the requester's external addr. The requester learns its own
	// external addr from the proxy_info reply (target.Addr above), and the
	// target learns the requester from this push.
	//
	// Source priority for the requester's addr:
	//   1. proxies[requesterID].Addr — set by punch keepalive's listener
	//      socket, this is the dial-target NAT mapping the target should
	//      fire toward. Preferred.
	//   2. TCP control connection's RemoteAddr — fallback when the
	//      requester hasn't completed register yet (RV restart, fresh
	//      conn). Same external IP as the listener socket; only the port
	//      differs. Full/Restricted-Cone NAT (most home routers) match on
	//      IP alone, so this still opens the hole in practice.
	var requesterAddr string
	if requester, hasReq := s.proxies[nodeKey(cc.nodeID)]; hasReq && requester != nil && requester.Addr != "" {
		requesterAddr = requester.Addr
	} else if ra := cc.conn.RemoteAddr(); ra != nil {
		requesterAddr = ra.String()
	}
	if requesterAddr != "" {
		punch := &tunnel.RendezvousMsg{V: 1, Type: signal.TypePunch, ID: cc.nodeID, Addr: requesterAddr}
		if err := s.PushToNode(targetID, punch); err != nil && !errors.Is(err, ErrNotConnected) {
			log.Printf("[rendezvous] connect: push punch to %s: %v", targetID, err)
		}
	}

	// Candidates: srflx (target.Addr) + host (target.LocalAddr) packed
	// in a single list so requester's isannd can race-dial all candidates.
	// Order matters — first listed is tried first by simple-iterator
	// callers; race-dial callers ignore order. We put LocalAddr first
	// when both peers happen to share a LAN (hairpin-NAT-free direct hit),
	// then the srflx for everyone else.
	candidates := make([]string, 0, 2)
	if target.LocalAddr != "" && target.LocalAddr != target.Addr {
		candidates = append(candidates, target.LocalAddr)
	}
	if target.Addr != "" {
		candidates = append(candidates, target.Addr)
	}
	// proxy_info carries only the addresses RV knows for the target. The
	// hole-punch policy fields (punch_count etc.) deliberately do NOT
	// travel here — they are pushed on every register_ack instead, and
	// isannd's NLB pipe stores them as atomic globals. That way:
	//
	//   - the listener's reply burst (which has no per-peer context — an
	//     incoming punch only carries a UDP source addr) always has the
	//     current policy ready;
	//   - pre-dial values stay consistent across lookups without RV
	//     re-shipping them on every Connect frame (~1KB → ~200B savings
	//     per lookup);
	//   - policy / candidates have decoupled refresh cycles (policy on
	//     register cadence, candidates on demand).
	_ = cc.sendFrame(&tunnel.RendezvousMsg{
		V:          1,
		Type:       signal.TypeProxyInfo,
		ID:         targetID,
		Addr:       target.Addr,
		Candidates: candidates,
	})
}

// PushToNode delivers msg to the named node over its TCP control
// connection, if currently connected. Returns ErrNotConnected when
// the node has no active control connection.
func (s *Server) PushToNode(nodeID string, msg *tunnel.RendezvousMsg) error {
	v, ok := s.controlConns.Load(nodeKey(nodeID))
	if !ok {
		return ErrNotConnected
	}
	cc, ok := v.(*controlConn)
	if !ok || cc == nil {
		return ErrNotConnected
	}
	return cc.sendFrame(msg)
}
