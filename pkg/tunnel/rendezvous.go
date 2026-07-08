package tunnel

import (
	"net"
	"strconv"
	"strings"

	"github.com/isannai/isann-servers/pkg/setup"
)

// CredentialMessage is the canonical string an issuer signs to authorize a
// node's admission to a protected RV. Binds the node EOA + issuance time + the
// absolute expiry so the credential can't be reused for a different node and the
// RV (and the node, via `isann cred list`) can tell when it has expired. The
// expiry is the sole lifetime control — a protected RV rejects expireMs ≤ 0.
// Issuer side:  sig = auth.SignMessage(CredentialMessage(nodeAddr, issuedMs, expireMs), issuerPK)
// RV side:      issuer = auth.RecoverAddress(CredentialMessage(nodeAddr, CredIssuedMs, CredExpireMs), CredSig)
func CredentialMessage(nodeAddr string, issuedMs, expireMs int64) string {
	return "ISANN-CREDENTIAL:" + strings.ToLower(nodeAddr) + ":" +
		strconv.FormatInt(issuedMs, 10) + ":" + strconv.FormatInt(expireMs, 10)
}

// ParseCredentialMessage extracts (node, issuedMs, expireMs) from an
// ISANN-CREDENTIAL message. node MAY be empty (bearer). ok=false on bad shape.
// The `isann cred add --token` path uses it to unpack the issued token.
func ParseCredentialMessage(msg string) (node string, issuedMs, expireMs int64, ok bool) {
	const prefix = "ISANN-CREDENTIAL:"
	if !strings.HasPrefix(msg, prefix) {
		return "", 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(msg, prefix), ":")
	if len(parts) != 3 {
		return "", 0, 0, false
	}
	node = strings.ToLower(strings.TrimSpace(parts[0])) // "" = bearer
	issuedMs, e1 := strconv.ParseInt(parts[1], 10, 64)
	expireMs, e2 := strconv.ParseInt(parts[2], 10, 64)
	if e1 != nil || e2 != nil {
		return "", 0, 0, false
	}
	return node, issuedMs, expireMs, true
}

// RendezvousMsg matches the rendezvous protocol.
type RendezvousMsg struct {
	V          int      `json:"v"`
	Type       string   `json:"type"`
	Role       string   `json:"role,omitempty"` // "provider" or "broker"
	ID         string   `json:"id,omitempty"`
	Addr       string   `json:"addr,omitempty"`
	LocalAddr  string   `json:"local_addr,omitempty"` // LAN address for same-NAT connections
	Candidates []string `json:"candidates,omitempty"` // ICE-lite: peer reachable addresses
	CertHash   string   `json:"cert_hash,omitempty"`

	// Hole-punch policy — RV ships this in `proxy_info` replies alongside
	// Candidates so the requesting isannd uses the parameters RV has tuned
	// for the current NAT mix. Zero values fall back to isannd's built-in
	// defaults (older RV that doesn't populate these — backward compat).
	//
	// PunchCount / PunchIntervalMs / PreDialWaitMs apply to the requester's
	// pre-dial punch round (outbound):
	//
	//	1. Fire PunchCount UDP packets at each candidate, PunchIntervalMs
	//	   apart, from the same socket the upcoming HTTP/3 dial will use.
	//	2. Sleep PreDialWaitMs so the requester's NAT has time to install
	//	   the mapping and the target's RV-coordinated reply punch reaches
	//	   the requester through the freshly-opened hole.
	//	3. Race-dial all candidates.
	//
	// ReplyBurstCount / ReplyBurstIntervalMs apply to the inbound listener's
	// reaction when a peer's punch arrives — fire ReplyBurstCount packets
	// back, ReplyBurstIntervalMs apart, to keep the NAT mapping alive until
	// the peer's HTTP/3 handshake reaches it.
	PunchCount           int `json:"punch_count,omitempty"`
	PunchIntervalMs      int `json:"punch_interval_ms,omitempty"`
	PreDialWaitMs        int `json:"pre_dial_wait_ms,omitempty"`
	ReplyBurstCount      int `json:"reply_burst_count,omitempty"`
	ReplyBurstIntervalMs int `json:"reply_burst_interval_ms,omitempty"`

	Version      string `json:"version,omitempty"`       // Provider version
	BinHash      string `json:"bin_hash,omitempty"`      // Provider binary hash
	OwnerAddress string `json:"owner_address,omitempty"` // Owner wallet address

	// Status removed (was redundant — heartbeat carries node-level status at 1Hz).
	Emblem   string              `json:"emblem,omitempty"`
	AuthMode string              `json:"auth_mode,omitempty"` // "public" | "protected" ("open" = legacy alias)
	Hardware *setup.HardwareSpec `json:"hardware,omitempty"`
	// Always serialized (no omitempty) — disabled state needs to overwrite
	// RV's cached list, even when the resulting list is empty. Without this,
	// an empty services slice would be omitted from JSON, RV would preserve
	// the previous list, and disabled services would never disappear.
	Services []setup.ServiceInfo `json:"services"`

	// Delta optimization fields. Provider sends FullSync=true for first
	// register and periodic resync (every N intervals); subsequent registers
	// omit unchanged static fields (Hardware/Emblem/Version/OwnerAddress).
	// Server preserves cached values for omitted fields when FullSync=false.
	Seq      uint64 `json:"seq,omitempty"`
	FullSync bool   `json:"full_sync,omitempty"`

	// Resync: rendezvous → provider ack when isNew (forces FullSync on next heartbeat)
	Resync bool `json:"resync,omitempty"`

	// TPM verification fields
	EKCert       []byte `json:"ek_cert,omitempty"`        // Provider → rendezvous: EK certificate DER
	AKName       []byte `json:"ak_name,omitempty"`        // Provider → rendezvous: AK name hash (for MakeCredential)
	TPMCredBlob  []byte `json:"tpm_cred_blob,omitempty"`  // Rendezvous → provider: credential blob
	TPMEncSecret []byte `json:"tpm_enc_secret,omitempty"` // Rendezvous → provider: encrypted secret
	TPMResponse  []byte `json:"tpm_response,omitempty"`   // Provider → rendezvous: decrypted secret
	TPMVerified  bool   `json:"tpm_verified,omitempty"`   // Rendezvous → provider: verification result

	// Session / ECDSA register authentication (Phase 1 refine).
	// Signature + TimestampMs are sent on FullSync registers so RV can
	// ecrecover the node's EOA and issue a session. TimestampMs binds
	// signature freshness (RV checks ±60s skew). SessionToken/Key come
	// back on the ack; the client uses SessionKey to HMAC subsequent UDP
	// heartbeats.
	Signature        []byte `json:"signature,omitempty"`          // 65-byte secp256k1 signature over RegisterDigest
	TimestampMs      int64  `json:"timestamp_ms,omitempty"`       // Unix ms used in digest
	SessionToken     []byte `json:"session_token,omitempty"`      // RV → node (16B UUID)
	SessionKey       []byte `json:"session_key,omitempty"`        // RV → node (32B HMAC key)
	SessionExpiresMs int64  `json:"session_expires_ms,omitempty"` // RV → node (Unix ms)

	// Protected-mode admission credential (FullSync register only). An issuer
	// listed in the RV's auth.json signs CredentialMessage(nodeAddr,
	// CredIssuedMs, CredExpireMs); isannd attaches the active credential from
	// its pool to the register frame. RV (protected mode) recovers the issuer
	// from CredSig, checks it's authorized, the bind shape is permitted, and
	// that now ≤ CredExpireMs (rejecting expireMs ≤ 0). Empty in public mode.
	// See CredentialMessage above + pkg/rendezvous admission gate.
	CredSig      string `json:"cred_sig,omitempty"`       // issuer's eth_sign hex over CredentialMessage
	CredIssuedMs int64  `json:"cred_issued_ms,omitempty"` // credential issuance time (signed; audit only)
	CredExpireMs int64  `json:"cred_expire_ms,omitempty"` // absolute expiry, Unix ms (signed; RV enforces now ≤ this)

	// Baseline ping cadence the RV wants this client to use, in seconds.
	// RV → node, sent on every register_ack. Provider/Broker reset their
	// ping ticker to this value. 0 = field absent (older RV) → client
	// falls back to a built-in default.
	PingIntervalSec uint32 `json:"ping_interval_sec,omitempty"`

	// Baseline register cadence the RV wants this client to use, in
	// seconds. RV → node, sent on every register_ack. Provider/Broker
	// reset their register ticker to this value so RV decides how often
	// fullSync re-syncs occur. 0 = field absent → client falls back to a
	// built-in default (300s).
	RegisterIntervalSec uint32 `json:"register_interval_sec,omitempty"`

	// ServiceEvent fields (Type="service_event"): a push sent by the
	// provider on the QUIC control stream whenever a local service
	// transitions between lifecycle states. Replaces HTTP polling of the
	// service /health endpoint as the authoritative liveness signal.
	Event       string         `json:"event,omitempty"`        // "service.starting" | "service.ready" | "service.stopped"
	ServiceName string         `json:"service_name,omitempty"` // e.g. "llm-api"
	ServiceInfo map[string]any `json:"service_info,omitempty"` // engine/model/version/addr/child_pid/load_time_ms/reason

	// Metrics — event-driven per-service metrics push (Type="service_event").
	// nil 이면 metrics 변화 없는 frame (예: ping). RV 가 받아서 proxies
	// 캐시의 per-service metrics 영역을 patch.
	//
	// Deprecated: single-service form. New code should populate
	// MetricsBatch with one or more entries — provider coalesces queue
	// callbacks across a short window and sends them as a batch. Receivers
	// must handle both fields (legacy providers may still emit Metrics).
	Metrics *ServiceMetrics `json:"metrics,omitempty"`

	// MetricsBatch — multi-service metrics snapshot sent in one frame.
	// Provider's pushAllMetrics filters out services with no value (e.g.
	// status=stopped + zero counters) and packs the remainder here. The
	// field intentionally omits `omitempty` so an empty `[]` survives
	// JSON marshalling — receivers can distinguish "provider pushed
	// metrics, nothing to report" from "frame has no batch field at
	// all" (= legacy single-Metrics form below).
	MetricsBatch []ServiceMetrics `json:"metrics_batch"`
}

// MustResolveUDP resolves a UDP address, returning an empty address on error.
func MustResolveUDP(addr string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return &net.UDPAddr{}
	}
	return a
}
