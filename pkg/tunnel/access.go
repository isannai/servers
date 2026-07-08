package tunnel

// access.go — inference-access credential: the issuer-signed grant that lets a
// caller use a node's /svc inference door (the data-plane analogue of the RV
// admission CredentialMessage in rendezvous.go — SAME shape, only the prefix and
// verifying door differ).
//
// Two layers, kept apart:
//   - AccessMessage   = the canonical string the issuer SIGNS:
//     "ISANN-ACCESS:<buyer>:<issued>:<expire>" — exactly RV's
//     "ISANN-CREDENTIAL:<node>:<issued>:<expire>" with a distinct prefix (so an RV
//     admission credential can never be replayed as an inference grant). There is
//     NO serving-node field: a grant works on whichever node trusts the issuer
//     (its auth.json) — issuer trust already scopes it, exactly like RV.
//   - Access token    = a single copy-paste blob "ianacc_<base64url(json)>"
//     bundling the message + signature, so an operator pastes ONE value into a
//     mesh/web config (vs four separate flags). The consumer sends it verbatim in
//     the X-ISANN-Credential header; for a BOUND grant (buyer set) it ALSO self-signs
//     the request (standard M0 Authorization + X-ISANN-Message) to prove identity.
//
// isannd's /svc gate decodes X-ISANN-Credential → verifies the MESSAGE (recover issuer
// → role + expiry; for a bound grant, request signer == buyer). Only the issuer
// (account issue) encodes and the consumer (mesh/web) sends the TOKEN. No new
// crypto — reuses auth.SignMessage / auth.RecoverAddress.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// accessPrefix marks an inference-access message. Distinct from the RV
// admission "ISANN-CREDENTIAL:" so the two doors can't share a credential.
const accessPrefix = "ISANN-ACCESS:"

// AccessTokenPrefix marks the single-blob copy-paste token form.
const AccessTokenPrefix = "ianacc_"

// AccessMessage is the canonical string an issuer signs to grant inference
// access to a node's /svc door. Three fields (RV's shape, different prefix):
//
//		ISANN-ACCESS:<buyer>:<issued>:<expire>
//
//	  - buyer  = WHO may use the grant. EMPTY = anonymous (bearer): anyone holding
//	    the grant may use it. SET = bound: ONLY that address may use it — the caller
//	    must additionally prove identity with their OWN per-request signature
//	    (standard M0), and the gate checks signer == buyer. So a leaked BOUND grant
//	    is useless without the buyer's private key.
//	  - issued/expire = lifetime; expiry is the sole control (gate rejects ≤0/past).
//
// No serving-node field: a grant works on whichever node trusts the issuer (its
// auth.json), exactly as RV's CredentialMessage works on whichever RV trusts it.
//
//	Issuer side: sig = auth.SignMessage(AccessMessage(buyer, issued, expire), issuerPK)
//	/svc side:   issuer = auth.RecoverAddress(AccessMessage(buyer, issued, expire), sig)
func AccessMessage(buyerAddr string, issuedMs, expireMs int64) string {
	return accessPrefix + strings.ToLower(buyerAddr) + ":" +
		strconv.FormatInt(issuedMs, 10) + ":" + strconv.FormatInt(expireMs, 10)
}

// IsAccessMessage reports whether msg carries the inference-access prefix — a
// utility predicate for distinguishing an AccessMessage from a per-request M0
// canonical. (The /svc gate routes on the X-ISANN-Credential header, not this.)
func IsAccessMessage(msg string) bool {
	return strings.HasPrefix(msg, accessPrefix)
}

// ParseAccessMessage extracts (buyer, issuedMs, expireMs) from an access
// message. buyer MAY be empty (bearer). ok=false when the prefix or shape is
// wrong — callers MUST treat that as "deny".
func ParseAccessMessage(msg string) (buyer string, issuedMs, expireMs int64, ok bool) {
	if !strings.HasPrefix(msg, accessPrefix) {
		return "", 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(msg, accessPrefix), ":")
	if len(parts) != 3 {
		return "", 0, 0, false
	}
	buyer = strings.ToLower(strings.TrimSpace(parts[0])) // "" = bearer (anonymous)
	issuedMs, err1 := strconv.ParseInt(parts[1], 10, 64)
	expireMs, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil {
		return "", 0, 0, false
	}
	return buyer, issuedMs, expireMs, true
}

// accessTokenPayload is the JSON bundled inside an access token.
type accessTokenPayload struct {
	Msg string `json:"m"` // the signed AccessMessage
	Sig string `json:"s"` // its signature (hex)
}

// EncodeAccessToken bundles a signed access message + signature into the single
// copy-paste token "ianacc_<base64url(json)>". The consumer decodes it back into
// the (message, signature) pair to present on /svc.
func EncodeAccessToken(message, sig string) string {
	b, _ := json.Marshal(accessTokenPayload{Msg: message, Sig: sig})
	return AccessTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// DecodeAccessToken reverses EncodeAccessToken, returning the signed message and
// its signature. Used by the consumer (mesh/web) to build the /svc headers.
func DecodeAccessToken(token string) (message, sig string, err error) {
	s := strings.TrimSpace(token)
	if !strings.HasPrefix(s, AccessTokenPrefix) {
		return "", "", fmt.Errorf("not an access token (missing %q prefix)", AccessTokenPrefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, AccessTokenPrefix))
	if err != nil {
		return "", "", fmt.Errorf("decode access token: %w", err)
	}
	var p accessTokenPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("parse access token: %w", err)
	}
	if p.Msg == "" || p.Sig == "" {
		return "", "", fmt.Errorf("access token missing message or signature")
	}
	return p.Msg, p.Sig, nil
}
