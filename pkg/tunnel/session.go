package tunnel

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SessionTTL is how long a session_token remains valid after issue.
const SessionTTL = 24 * time.Hour

// SessionGrace is the extra window during which a rotated token is still
// honored alongside the newly issued one, smoothing over clock drift and
// rotation races.
const SessionGrace = 5 * time.Minute

// SessionRenewBefore is how early before expiry the client should begin
// re-signing a FullSync to rotate its token.
const SessionRenewBefore = 30 * time.Minute

// Session describes the token + HMAC key pair issued by the rendezvous
// server to an authenticated node. The node uses SessionKey to HMAC UDP
// heartbeat bodies; RV verifies by looking up SessionToken → SessionKey.
//
// Session is kept in memory only on both sides (never persisted). Each
// fresh QUIC register produces a new Session.
//
// HeartbeatInterval is the baseline tick rate the RV wants this client to
// emit heartbeats at. Zero means "unset, use built-in default" — older RV
// servers that don't carry the field leave this at zero on receive, which
// the heartbeat loop interprets as fallback.
type Session struct {
	Token             []byte // 16 bytes (UUID v4 binary form)
	Key               []byte // 32 bytes random (HMAC-SHA256 key)
	IssuedAt          time.Time
	ExpiresAt         time.Time
	HeartbeatInterval time.Duration
}

// NewSession produces a freshly-random Session with the conventional TTL.
func NewSession() (*Session, error) {
	tok := uuid.New()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("session: random key: %w", err)
	}
	now := time.Now()
	return &Session{
		Token:     tok[:],
		Key:       key,
		IssuedAt:  now,
		ExpiresAt: now.Add(SessionTTL),
	}, nil
}

// IsExpired reports whether the session expired outside the grace window.
func (s *Session) IsExpired(now time.Time) bool {
	if s == nil {
		return true
	}
	return now.After(s.ExpiresAt.Add(SessionGrace))
}

// NeedsRenew returns true once the token is within SessionRenewBefore of
// its stated expiry. Callers use this to schedule a new FullSync.
func (s *Session) NeedsRenew(now time.Time) bool {
	if s == nil {
		return true
	}
	return now.After(s.ExpiresAt.Add(-SessionRenewBefore))
}

// RegisterDigest is the canonical hash clients sign when sending a FullSync
// register. Matches the bytes RV recomputes for ecrecover.
//
// Inputs are serialized in a fixed order with length prefixes so unrelated
// fields can't be confused (length-delimited canonical form). Replay
// freshness is bound by timestampMs (RV checks ±60s skew on receive).
func RegisterDigest(nodeID, certHash, version, binHash, ownerAddress, hardwareHash string, timestampMs int64) []byte {
	h := sha256.New()
	appendField := func(label, value string) {
		h.Write([]byte(label))
		var lp [4]byte
		binary.BigEndian.PutUint32(lp[:], uint32(len(value)))
		h.Write(lp[:])
		h.Write([]byte(value))
	}
	appendField("id", nodeID)
	appendField("cert", certHash)
	appendField("ver", version)
	appendField("bin", binHash)
	appendField("own", ownerAddress)
	appendField("hw", hardwareHash)

	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(timestampMs))
	h.Write([]byte("ts"))
	h.Write(ts[:])

	return h.Sum(nil)
}

// PingDigest is what backends sign on every liveness ping. Intentionally
// omits the node id — RV recovers the signer's EOA via ecrecover and
// reconstructs nodeID = role_prefix + EOA, so encoding it in the message
// would be redundant. Including role binds the signature to the role
// claim (a wallet can't reuse the same ping under both P: and B:).
// timestampMs is wall-clock at send time; RV rejects when skew exceeds
// PingTimestampSkew (60s by default).
func PingDigest(role string, timestampMs int64) []byte {
	h := sha256.New()
	h.Write([]byte("role"))
	var rp [4]byte
	binary.BigEndian.PutUint32(rp[:], uint32(len(role)))
	h.Write(rp[:])
	h.Write([]byte(role))
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(timestampMs))
	h.Write([]byte("ts"))
	h.Write(ts[:])
	return h.Sum(nil)
}

// SessionCache holds active sessions on the rendezvous side. Sessions are
// indexed both by token (for UDP heartbeat lookup) and by node ID (for
// control-plane lookups and pre-expiry replacement).
type SessionCache struct {
	mu       sync.RWMutex
	byToken  map[string]*SessionEntry // token hex → entry
	byNodeID map[string]*SessionEntry // node_id    → latest entry
	replaced map[string]time.Time     // token hex → grace-end time (previous session after rotation)
}

// SessionEntry is one cached session plus its owning node ID.
type SessionEntry struct {
	NodeID string
	Sess   *Session
}

// NewSessionCache returns an empty cache.
func NewSessionCache() *SessionCache {
	return &SessionCache{
		byToken:  make(map[string]*SessionEntry),
		byNodeID: make(map[string]*SessionEntry),
		replaced: make(map[string]time.Time),
	}
}

// Put stores a fresh session for nodeID, replacing any earlier one. The
// previous token (if any) is moved to the grace set so in-flight heartbeat
// packets still validate for SessionGrace.
func (c *SessionCache) Put(nodeID string, sess *Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.byNodeID[nodeID]; old != nil {
		c.replaced[tokenKey(old.Sess.Token)] = time.Now().Add(SessionGrace)
	}
	e := &SessionEntry{NodeID: nodeID, Sess: sess}
	c.byNodeID[nodeID] = e
	c.byToken[tokenKey(sess.Token)] = e
}

// LookupByToken returns the active session matching token, or nil if not
// found / expired. A session in the grace window is still returned.
func (c *SessionCache) LookupByToken(token []byte) *SessionEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	k := tokenKey(token)
	if e, ok := c.byToken[k]; ok {
		if !e.Sess.IsExpired(time.Now()) {
			return e
		}
	}
	if end, ok := c.replaced[k]; ok {
		if time.Now().Before(end) {
			// Return via byToken map entry (may have been overwritten already).
			if e, ok := c.byToken[k]; ok {
				return e
			}
		}
	}
	return nil
}

// LookupByNodeID returns the latest session for a node.
func (c *SessionCache) LookupByNodeID(nodeID string) *SessionEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if e, ok := c.byNodeID[nodeID]; ok {
		if !e.Sess.IsExpired(time.Now()) {
			return e
		}
	}
	return nil
}

// Prune removes expired sessions (outside grace). Safe to call periodically.
func (c *SessionCache) Prune(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.byToken {
		if e.Sess.IsExpired(now) {
			delete(c.byToken, k)
		}
	}
	for id, e := range c.byNodeID {
		if e.Sess.IsExpired(now) {
			delete(c.byNodeID, id)
		}
	}
	for k, end := range c.replaced {
		if now.After(end) {
			delete(c.replaced, k)
		}
	}
}

func tokenKey(tok []byte) string {
	// Tokens are 16B UUIDs; just reinterpret.
	return string(tok)
}

// UUIDString returns a human-friendly form of a binary token (for logs).
func UUIDString(tok []byte) string {
	if len(tok) != 16 {
		return fmt.Sprintf("(%d bytes)", len(tok))
	}
	u, err := uuid.FromBytes(tok)
	if err != nil {
		return "invalid"
	}
	return u.String()
}
