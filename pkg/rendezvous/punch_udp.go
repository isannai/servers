package rendezvous

import (
	"context"
	"log"
	"net"
)

// Punch UDP packet prefixes — the wire format for isannd → RV pings on the
// unified port's UDP transport. Designed to share buffer-space ideas with
// the legacy Addr port mux (0x01 / 0x02) so future de-duplication is easy.
const (
	punchPrefixPing      byte = 0x01 // isannd → RV: "this is my NAT mapping"
	punchPrefixHeartbeat byte = 0x02 // legacy heartbeat — ignored here, handled by Addr
)

// runPunchUDP serves the Phase 3 unified port's UDP transport. Two jobs:
//
//  1. Punch ping from isannd (0x01 prefix + node_id) — learn / refresh the
//     node's external NAT mapping. The source address of this packet is
//     the address other peers will dial later, so it's the source of truth
//     for ProxyInfo.Addr / .UDPAddr.
//
//  2. (Optional) legacy heartbeat (0x02) — ignored here; the existing
//     handleHeartbeats on s.Addr already processes those. Future work can
//     fold heartbeat into the TCP control channel and retire this path.
//
// Peer-to-peer data does NOT flow through this listener — peers exchange
// HTTP/3 directly with each other once the NAT hole is open.
func (s *Server) runPunchUDP(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	if uc, ok := pc.(*net.UDPConn); ok {
		_ = uc.SetReadBuffer(4 * 1024 * 1024)
	}
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()
	log.Printf("[rendezvous] UDP punch listening on %s", addr)

	buf := make([]byte, 1500)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("[rendezvous] UDP punch read: %v", err)
			continue
		}
		if n < 2 {
			continue
		}
		switch buf[0] {
		case punchPrefixPing:
			nodeID := string(buf[1:n])
			if nodeID == "" {
				continue
			}
			s.learnPublicAddr(nodeID, src)
		case punchPrefixHeartbeat:
			// Legacy — ignored on this listener
		default:
			log.Printf("[rendezvous] UDP punch: unknown prefix 0x%02x from %s", buf[0], src)
		}
	}
}

// learnPublicAddr updates ProxyInfo.Addr / .UDPAddr from the source of a
// punch ping. The node must already have a registry entry (created by the
// TCP control connection's register frame); we don't create entries here
// — a UDP packet alone is not authenticated.
//
// Security: the punch packet's source IP must match the TCP control
// connection's source IP (ControlIP, bound at register time). Without
// this any third party could send `[0x01][P:0xABC]` and redirect the
// NAT mapping, MITM-ing peer dials to that node.
func (s *Server) learnPublicAddr(nodeID string, src net.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proxies[nodeKey(nodeID)]
	if !ok {
		// TCP register hasn't happened yet — punch arrived early.
		// isannd will retry, so just log and drop.
		log.Printf("[rendezvous] UDP punch: source %s declared id=%q but no registry entry yet", src, nodeID)
		return
	}
	udpAddr, ok := src.(*net.UDPAddr)
	if !ok {
		return
	}
	// Reject when the punch source IP differs from the TCP control IP.
	// ControlIP unset = legacy register path; skip the check then.
	if len(p.ControlIP) > 0 && !udpAddr.IP.Equal(p.ControlIP) {
		log.Printf("[rendezvous] UDP punch: src %s does not match control IP %s for %s — reject (possible spoof)", udpAddr.IP, p.ControlIP, nodeID)
		return
	}
	p.UDPAddr = udpAddr
	// Respect explicit msg.Addr from register. Production NAT setups
	// leave AddrManual=false so live punch source becomes the dial target.
	if !p.AddrManual {
		p.Addr = src.String()
	}
	// LastSeen intentionally NOT touched here — punch ping comes from isannd
	// (transport layer) and only signals "isannd is alive / NAT mapping
	// still valid". Node liveness = backend (provider/broker) alive, which
	// is the ping/register/service_event TCP frames in control_tcp.go.
	// Updating LastSeen here would keep dead-backend nodes alive forever as
	// long as isannd keeps punching.
}
