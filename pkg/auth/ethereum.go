package auth

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// BuildCanonical assembles the canonical string that cross-node admin
// requests (Phase 2 §6) sign and verify. Binding METHOD + PATH(+query) +
// body hash + target nodeID + nonce + timestamp closes cross-endpoint,
// cross-node, and body-tamper replay. Signer (local isannd / browser wallet)
// and verifier (target ingress) MUST call this same function so the signed
// bytes match exactly — a divergence here is the walletIsOperator-class bug
// risk, so there is one builder, used by both sides.
func BuildCanonical(method, pathQuery, bodyHashHex, targetNodeID, nonce, timestamp string) string {
	return strings.Join([]string{method, pathQuery, bodyHashHex, targetNodeID, nonce, timestamp}, "\n")
}

// SignMessage signs message with the eth_sign convention (\x19 prefix +
// keccak256) using pk, returning 0x-less hex of r‖s‖v with v in {27,28}.
// Exact inverse of RecoverAddress (same prefix, same hash, matching V offset),
// so SignMessage→RecoverAddress round-trips to pk's address. eth_sign /
// personal_sign compatible, so a browser wallet (MetaMask) signature over the
// same canonical verifies identically. Reuses go-ethereum crypto.Sign — no
// hand-rolled curve math.
func SignMessage(message string, pk *ecdsa.PrivateKey) (string, error) {
	prefix := fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(message))
	hash := crypto.Keccak256([]byte(prefix + message))
	sig, err := crypto.Sign(hash, pk)
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	sig[64] += 27 // crypto.Sign yields V in {0,1}; RecoverAddress expects {27,28}
	return hex.EncodeToString(sig), nil
}

// RecoverAddress recovers the signer's Ethereum address from a message and signature.
func RecoverAddress(message, signatureHex string) (string, error) {
	sigHex := strings.TrimPrefix(signatureHex, "0x")
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return "", fmt.Errorf("invalid signature hex: %w", err)
	}
	if len(sigBytes) != 65 {
		return "", fmt.Errorf("signature must be 65 bytes, got %d", len(sigBytes))
	}
	if sigBytes[64] >= 27 {
		sigBytes[64] -= 27
	}
	prefix := fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(message))
	hash := crypto.Keccak256([]byte(prefix + message))
	pubKey, err := crypto.SigToPub(hash, sigBytes)
	if err != nil {
		return "", fmt.Errorf("failed to recover public key: %w", err)
	}
	return crypto.PubkeyToAddress(*pubKey).Hex(), nil
}

func VerifySignature(message, signatureHex, expectedAddress string) (bool, error) {
	sigHex := strings.TrimPrefix(signatureHex, "0x")
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return false, fmt.Errorf("invalid signature hex: %w", err)
	}
	if len(sigBytes) != 65 {
		return false, fmt.Errorf("signature must be 65 bytes, got %d", len(sigBytes))
	}
	if sigBytes[64] >= 27 {
		sigBytes[64] -= 27
	}
	prefix := fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(message))
	hash := crypto.Keccak256([]byte(prefix + message))
	pubKey, err := crypto.SigToPub(hash, sigBytes)
	if err != nil {
		return false, fmt.Errorf("failed to recover public key: %w", err)
	}
	return crypto.PubkeyToAddress(*pubKey) == common.HexToAddress(expectedAddress), nil
}
