package market

// auth_write.go — author-signed write/read verification (market-server.md §4,
// §10.3). Same canonical model as the isannd admin door: the signature binds
// METHOD + PATH(+query) + sha256(body) + target + nonce + timestamp, so a
// signed recipe can't be replayed onto another type/name, the body can't be
// tampered, and nonce/ts close replay. We reuse pkg/auth — NO new crypto.
//
//	canonical = BuildCanonical(method, RequestURI, sha256hex(body), target, nonce, ts)
//	target    = lower(author) for /v1/assets/{type}/{author}/{name}[...]
//	          = "mine" for /v1/assets/mine (no path author; recovered addr IS the author)

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/isannai/isann-servers/pkg/auth"
)

// nonceCache remembers spent nonces until their signing timestamp ages out of
// the replay window, so a captured request can't be replayed within it.
type nonceCache struct {
	mu   sync.Mutex
	seen map[string]int64 // nonce → unix expiry
}

func newNonceCache() *nonceCache { return &nonceCache{seen: map[string]int64{}} }

// useOnce records nonce with the given expiry; returns false if already spent.
func (c *nonceCache) useOnce(nonce string, expiry int64, nowUnix int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Opportunistic sweep of expired entries.
	for k, exp := range c.seen {
		if exp < nowUnix {
			delete(c.seen, k)
		}
	}
	if _, dup := c.seen[nonce]; dup {
		return false
	}
	c.seen[nonce] = expiry
	return true
}

// verifyWrite validates the write/read signature over target and returns the
// recovered signer address (lowercase). On failure it writes the HTTP error
// and returns "".
func (s *Server) verifyWrite(w http.ResponseWriter, r *http.Request, body []byte, target string) string {
	// LOCAL DEV: skip all signature/nonce/timestamp checks. The "signer" is
	// simply the target (= path author for asset endpoints), so signer==author
	// gates still pass. Never enable in production.
	if s.cfg.DevInsecureSkipAuth {
		return strings.ToLower(target)
	}
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "ISANN ") {
		writeJSON(w, http.StatusUnauthorized, errBody("unauthorized", "Authorization: ISANN <sig> required"))
		return ""
	}
	sig := strings.TrimSpace(strings.TrimPrefix(authHeader, "ISANN "))
	nonce := r.Header.Get("X-ISANN-Nonce")
	tsStr := r.Header.Get("X-ISANN-Timestamp")
	if nonce == "" || tsStr == "" {
		writeJSON(w, http.StatusUnauthorized, errBody("unauthorized", "X-ISANN-Nonce and X-ISANN-Timestamp required"))
		return ""
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errBody("unauthorized", "invalid timestamp"))
		return ""
	}
	now := time.Now().Unix()
	win := int64(s.cfg.ReplayWinS)
	if ts < now-win || ts > now+win {
		writeJSON(w, http.StatusUnauthorized, errBody("unauthorized", "timestamp outside replay window"))
		return ""
	}

	bodyHash := sha256.Sum256(body)
	canonical := auth.BuildCanonical(r.Method, r.URL.RequestURI(), hex.EncodeToString(bodyHash[:]), strings.ToLower(target), nonce, tsStr)
	addr, err := auth.RecoverAddress(canonical, sig)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errBody("unauthorized", "signature recover failed: "+err.Error()))
		return ""
	}
	if !s.nonces.useOnce(nonce, ts+win, now) {
		writeJSON(w, http.StatusUnauthorized, errBody("unauthorized", "nonce already used"))
		return ""
	}
	return strings.ToLower(addr)
}
