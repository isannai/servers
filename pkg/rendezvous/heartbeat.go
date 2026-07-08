package rendezvous

import (
	"time"
)

// Legacy 0x02 UDP heartbeat handlers (handleHeartbeats / processHeartbeat
// / sendResyncHint) were removed alongside the legacy UDP+HTTP/3 listener.
// Heartbeat metrics now arrive via TCP control frames in
// pkg/rendezvous/control_tcp.go (applyServiceEvent → s.metrics).
//
// The aggregate/lookup helpers below remain because the REST handlers
// (/v1/nodes, /v1/metrics) still call into them.

// aggregateNodeStatus walks the metric cache for nodeID and derives the
// top-level node status. Priority: busy > loading > idle > standby.
//
// Returns "" when no metrics exist yet (caller preserves prior status) or
// when the node itself is stale (LastSeen > 5s old) so a crashed service
// doesn't leave a permanent BUSY.
func (s *Server) aggregateNodeStatus(nodeID string) string {
	// Snapshot LastSeen under proxy lock first; never hold both locks.
	s.mu.RLock()
	var lastSeen time.Time
	if p := s.proxies[nodeKey(nodeID)]; p != nil {
		lastSeen = p.LastSeen
	}
	s.mu.RUnlock()
	if !lastSeen.IsZero() && time.Since(lastSeen) > 5*time.Second {
		return ""
	}

	s.metricsMu.RLock()
	defer s.metricsMu.RUnlock()

	prefix := nodeID + "/"
	var anyBusy, anyIdle, anyLoading, any bool
	for k, m := range s.metrics {
		if m.NodeID != nodeID {
			// key may still match; skip cheaply
			if len(k) < len(prefix) || k[:len(prefix)] != prefix {
				continue
			}
		}
		any = true
		switch heartbeatStatus(m.Status) {
		case "busy":
			anyBusy = true
		case "idle":
			anyIdle = true
		case "loading":
			anyLoading = true
		}
	}
	if !any {
		return ""
	}
	switch {
	case anyBusy:
		return "busy"
	case anyLoading:
		return "loading"
	case anyIdle:
		return "idle"
	default:
		return "standby"
	}
}

// heartbeatStatus maps the protobuf Status enum (as int32) to the
// string used by proxies[].Status and returned on /v1/nodes.
func heartbeatStatus(v int32) string {
	switch v {
	case 1:
		return "idle"
	case 2:
		return "busy"
	case 3:
		return "loading"
	case 4:
		return "stopped"
	default:
		return ""
	}
}

// GetMetric returns the latest cached heartbeat for the given (nodeID,
// serviceName) pair, or nil if nothing has arrived.
func (s *Server) GetMetric(nodeID, serviceName string) *NodeMetric {
	s.metricsMu.RLock()
	defer s.metricsMu.RUnlock()
	return s.metrics[metricKey(nodeID, serviceName)]
}

// pruneMetrics removes metric entries whose owning node's last heartbeat
// (proxies[nodeID].LastSeen) is older than d. Also drops orphans whose
// proxy entry was already deleted.
func (s *Server) pruneMetrics(d time.Duration) {
	// Snapshot LastSeen per node first to avoid holding both locks at once.
	s.mu.RLock()
	live := make(map[string]time.Time, len(s.proxies))
	for id, p := range s.proxies {
		live[id] = p.LastSeen
	}
	s.mu.RUnlock()

	cutoff := time.Now().Add(-d)
	s.metricsMu.Lock()
	for k, m := range s.metrics {
		ls, ok := live[m.NodeID]
		if !ok || ls.Before(cutoff) {
			delete(s.metrics, k)
		}
	}
	s.metricsMu.Unlock()
}
