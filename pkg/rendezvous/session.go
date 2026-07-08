package rendezvous

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/isannai/isann-servers/pkg/setup"
	"github.com/isannai/isann-servers/pkg/tunnel"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// hashHardwareForVerify mirrors the provider-side hardware hash so the
// server can recompute the same RegisterDigest that the client signed.
// Must stay byte-identical to pkg/station/rendezvous.go:hashHardware.
func hashHardwareForVerify(hw *setup.HardwareSpec) string {
	if hw == nil {
		return ""
	}
	data, err := json.Marshal(hw)
	if err != nil {
		return ""
	}
	const offset uint64 = 14695981039346656037
	const prime uint64 = 1099511628211
	h := offset
	for _, b := range data {
		h ^= uint64(b)
		h *= prime
	}
	return fmt.Sprintf("%016x", h)
}

// verifyRegisterSignature checks that Signature was produced by the private
// key matching the address embedded in msg.ID ("P:0x..." / "B:0x...").
//
// Rejects stale signatures (±60s from server clock).
func verifyRegisterSignature(msg *tunnel.RendezvousMsg, hwHash string) error {
	if len(msg.Signature) != 65 {
		return errors.New("signature length must be 65")
	}
	if msg.TimestampMs == 0 {
		return errors.New("timestamp missing")
	}
	now := time.Now().UnixMilli()
	skew := now - msg.TimestampMs
	if skew < 0 {
		skew = -skew
	}
	if skew > 60_000 {
		return fmt.Errorf("timestamp skew too large (%d ms)", skew)
	}

	expectedAddr, err := addrFromNodeID(msg.ID)
	if err != nil {
		return err
	}

	digest := tunnel.RegisterDigest(msg.ID, msg.CertHash, msg.Version, msg.BinHash, msg.OwnerAddress, hwHash, msg.TimestampMs)
	pub, err := crypto.SigToPub(digest, msg.Signature)
	if err != nil {
		return fmt.Errorf("ecrecover: %w", err)
	}
	recovered := crypto.PubkeyToAddress(*pub)
	if !bytes.EqualFold(recovered.Bytes(), expectedAddr.Bytes()) {
		return fmt.Errorf("signer %s != expected %s", recovered.Hex(), expectedAddr.Hex())
	}
	return nil
}

// nodeKey normalizes a "<Role>:<0xAddr>" id for use as the proxies /
// controlConns map key. Ethereum addresses are case-insensitive (case is only
// the EIP-55 checksum), yet callers send mixed forms — checksummed from
// .Hex(), lowercased by `isann favorite add`, etc. Keying by uppercase-role +
// lowercase-address makes register / connect / push case-agnostic so every
// form resolves the same node. ProxyInfo.ID is stored normalized too so
// `list nodes` shows one consistent (lowercase-address) form. The original id
// stays in the register frame's msg.ID — used for the RegisterDigest signature
// and the conn-binding check (msg.ID == controlConn.nodeID) — so signing and
// binding are untouched.
func nodeKey(id string) string {
	if i := strings.IndexByte(id, ':'); i >= 0 {
		return strings.ToUpper(id[:i]) + ":" + strings.ToLower(id[i+1:])
	}
	return strings.ToLower(id)
}

// addrFromNodeID extracts the EOA from a "P:0x..." / "B:0x..." id.
func addrFromNodeID(id string) (common.Address, error) {
	s := id
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	if !common.IsHexAddress(s) {
		return common.Address{}, fmt.Errorf("invalid node id %q", id)
	}
	return common.HexToAddress(s), nil
}

// verifyPingSignature checks that msg.Signature was produced by the
// private key matching the EOA inside msg.ID, over PingDigest(role,
// timestampMs). Rejects stale timestamps (±60s).
func verifyPingSignature(msg *tunnel.RendezvousMsg) error {
	if len(msg.Signature) != 65 {
		return errors.New("signature length must be 65")
	}
	if msg.TimestampMs == 0 {
		return errors.New("timestamp missing")
	}
	now := time.Now().UnixMilli()
	skew := now - msg.TimestampMs
	if skew < 0 {
		skew = -skew
	}
	if skew > 60_000 {
		return fmt.Errorf("timestamp skew too large (%d ms)", skew)
	}
	expectedAddr, err := addrFromNodeID(msg.ID)
	if err != nil {
		return err
	}
	digest := tunnel.PingDigest(msg.Role, msg.TimestampMs)
	pub, err := crypto.SigToPub(digest, msg.Signature)
	if err != nil {
		return fmt.Errorf("ecrecover: %w", err)
	}
	recovered := crypto.PubkeyToAddress(*pub)
	if !bytes.EqualFold(recovered.Bytes(), expectedAddr.Bytes()) {
		return fmt.Errorf("signer %s != expected %s", recovered.Hex(), expectedAddr.Hex())
	}
	return nil
}

// metricKey is the cache key for /v1/nodes metric merging.
// Keyed by nodeID only — a provider runs at most one service at a time,
// so each heartbeat just replaces the previous metric for that node.
func metricKey(nodeID, _ string) string {
	return nodeID
}
