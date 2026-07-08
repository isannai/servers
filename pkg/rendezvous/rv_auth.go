package rendezvous

// rv_auth.go — RV admission config. The RV (deployed separately from any
// provider/node) reads its OWN auth.json sitting next to rendezvous.json:
//
//	{
//	  "mode": "protected",                          // "public" (default) | "protected"
//	  "issuers": [ { "address": "0x..", "bind": "node" } ]
//	}
//
// public mode  — anyone with a valid register signature may register (current
//                behavior; issuers ignored).
// protected mode — a registering node must present an issuer-signed admission
//                credential (RendezvousMsg.CredSig over tunnel.CredentialMessage).
//                RV recovers the issuer, checks it's listed here, the bind shape
//                is permitted, and that the credential has not passed its signed
//                absolute expiry (CredExpireMs).
//
// Credential lifetime is the issuer's signed absolute expiry (CredExpireMs),
// which the node can read directly (so `isann cred list` shows valid/expired).
// There is no separate RV-side ttl ceiling — trust is binary: an issuer listed
// here is trusted to set its own expiry; revocation = removing it from this list.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/isannai/isann-servers/pkg/auth"
	"github.com/isannai/isann-servers/pkg/tunnel"
)

type rvAuthFile struct {
	Mode    string     `json:"mode"`
	Issuers []rvIssuer `json:"issuers"`
}

type rvIssuer struct {
	Address string `json:"address"`
	// Bind — what shape of credential this issuer may present:
	//   "node" (default) — credential bound to a specific node EOA; that node only.
	//   "none"           — bearer: issuer-signed only, ANY node id passes.
	//   "any"            — both bound and bearer accepted.
	Bind string `json:"bind,omitempty"`
}

// IssuerPolicy is the RV-side trust policy for one authorized admission issuer.
type IssuerPolicy struct {
	Bind string // "node" | "none" | "any"
}

func (p IssuerPolicy) allowsBound() bool  { return p.Bind == "node" || p.Bind == "any" }
func (p IssuerPolicy) allowsBearer() bool { return p.Bind == "none" || p.Bind == "any" }

// LoadRVAuth reads auth.json from the directory of the rendezvous config path.
// Returns the admission mode ("public" default) and the issuer allowlist keyed
// by lowercased issuer address → max credential age. A missing file means
// public mode with no issuers. "open" is a legacy alias for "public".
func LoadRVAuth(configPath string) (mode string, issuers map[string]IssuerPolicy, err error) {
	issuers = map[string]IssuerPolicy{}
	mode = "public"
	if configPath == "" {
		return mode, issuers, nil
	}
	path := filepath.Join(filepath.Dir(configPath), "auth.json")
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return mode, issuers, nil
		}
		return mode, issuers, fmt.Errorf("read %s: %w", path, rerr)
	}
	var f rvAuthFile
	if jerr := json.Unmarshal(data, &f); jerr != nil {
		return mode, issuers, fmt.Errorf("parse %s: %w", path, jerr)
	}
	mode = strings.ToLower(strings.TrimSpace(f.Mode))
	if mode == "" || mode == "open" {
		mode = "public"
	}
	if mode != "public" && mode != "protected" {
		return mode, issuers, fmt.Errorf("%s: invalid mode %q (want public|protected)", path, f.Mode)
	}
	for _, is := range f.Issuers {
		addr := strings.ToLower(strings.TrimSpace(is.Address))
		if addr == "" {
			continue
		}
		bind := strings.ToLower(strings.TrimSpace(is.Bind))
		if bind == "" {
			bind = "node"
		}
		if bind != "node" && bind != "none" && bind != "any" {
			return mode, issuers, fmt.Errorf("%s: issuer %s invalid bind %q (want node|none|any)", path, addr, is.Bind)
		}
		issuers[addr] = IssuerPolicy{Bind: bind}
	}
	if mode == "protected" && len(issuers) == 0 {
		// Fail loud, not silently closed: a protected RV with no issuers would
		// reject every node — almost always a misconfiguration.
		return mode, issuers, fmt.Errorf("%s: mode=protected but no issuers configured", path)
	}
	return mode, issuers, nil
}

// modeProtected reports whether the RV is in protected admission mode.
func (s *Server) modeProtected() bool { return strings.EqualFold(s.Mode, "protected") }

// admitRegister verifies a protected-mode admission credential carried on a
// FullSync register. It recovers the issuer from msg.CredSig over
// tunnel.CredentialMessage(nodeAddr, msg.CredIssuedMs, msg.CredExpireMs),
// requires that issuer to be listed in s.Issuers, the bind shape to be
// permitted, and the credential's signed absolute expiry (CredExpireMs) to be
// present and not yet passed. Returns nil when admitted, else the reason.
//
// Lifetime = the issuer's signed CredExpireMs alone (no separate RV ttl
// ceiling). The issuer is trusted to set its own expiry; the node can read it
// (`isann cred list` shows valid/expired). Revocation = removing the issuer
// from auth.json. CredIssuedMs is kept signed for audit only.
//
// Reuses pkg/auth.RecoverAddress (no new crypto). The node's own register
// signature is verified separately (verifyRegisterSignature) BEFORE this, so
// msg.ID is the node's real EOA by the time we bind the credential to it.
func (s *Server) admitRegister(msg *tunnel.RendezvousMsg) error {
	if msg.CredSig == "" {
		return errors.New("admission credential required")
	}
	if msg.CredExpireMs <= 0 {
		return errors.New("admission credential has no expiry")
	}
	addr, err := addrFromNodeID(msg.ID)
	if err != nil {
		return fmt.Errorf("node addr: %w", err)
	}
	// Two credential shapes share one signature, differing only in the node
	// binding of the signed message:
	//   node-bound : issuer signed CredentialMessage(nodeAddr, issued, expire) — that node only
	//   bearer     : issuer signed CredentialMessage("",       issued, expire) — any node id
	// Reconstruct each; whichever recovers to an authorized issuer whose policy
	// permits that shape wins. The signature enforces correctness — a bound
	// credential never recovers to a real issuer under the bearer message and
	// vice versa, so trying both can't be gamed.
	attempts := []struct {
		msg   string
		bound bool
	}{
		{tunnel.CredentialMessage(addr.Hex(), msg.CredIssuedMs, msg.CredExpireMs), true},
		{tunnel.CredentialMessage("", msg.CredIssuedMs, msg.CredExpireMs), false},
	}
	for _, a := range attempts {
		issuer, rerr := auth.RecoverAddress(a.msg, msg.CredSig)
		if rerr != nil {
			continue
		}
		pol, ok := s.Issuers[strings.ToLower(issuer)]
		if !ok {
			continue
		}
		if a.bound && !pol.allowsBound() {
			continue
		}
		if !a.bound && !pol.allowsBearer() {
			continue
		}
		if time.Now().UnixMilli() > msg.CredExpireMs {
			return fmt.Errorf("credential expired (issuer %s)", issuer)
		}
		return nil // admitted
	}
	return errors.New("no authorized issuer credential (unknown issuer or wrong bind)")
}
