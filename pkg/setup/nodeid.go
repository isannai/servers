package setup

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"

	"github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/crypto/hkdf"
)

// NodeIdentity represents a deterministic node identity derived from hardware.
type NodeIdentity struct {
	Address        string `json:"address"`                    // EOA: 0x...
	MainboardUUID  string `json:"mainboard_uuid"`             // System UUID
	TPMFingerprint string `json:"tpm_fingerprint,omitempty"`  // fTPM EK SHA256 (when available)
	GPUUID         string `json:"gpu_uuid,omitempty"`         // Fallback (when no TPM)
	EKCertRaw      []byte `json:"-"`                          // EK certificate DER (for TPM verification)
	EKCertIssuer   string `json:"ek_cert_issuer,omitempty"`   // EK cert issuer CN
	privateKey     *ecdsa.PrivateKey
}

// DeriveNodeIdentity derives a deterministic EOA address from hardware fingerprint.
//
// Priority:
//  1. fTPM EK fingerprint + Mainboard UUID
//  2. GPU UUID + Mainboard UUID (fallback if no TPM)
//
// The private key is re-derived each time from hardware — never stored on disk.
func DeriveNodeIdentity() (NodeIdentity, error) {
	mainboardUUID := fetchMainboardUUID()

	// Try fTPM first
	tpmInfo, tpmErr := ReadTPMInfo()
	if tpmErr == nil && tpmInfo.Fingerprint != "" {
		log.Printf("[nodeid] using fTPM EK + mainboard UUID")
		privKey, err := derivePrivateKey(mainboardUUID, tpmInfo.Fingerprint)
		if err != nil {
			return NodeIdentity{}, err
		}
		return NodeIdentity{
			Address:        crypto.PubkeyToAddress(privKey.PublicKey).Hex(),
			MainboardUUID:  mainboardUUID,
			TPMFingerprint: tpmInfo.Fingerprint,
			EKCertRaw:      tpmInfo.EKCertRaw,
			EKCertIssuer:   tpmInfo.EKCertIssuer,
			privateKey:     privKey,
		}, nil
	}

	// Fallback: GPU UUID
	log.Printf("[nodeid] fTPM not available (%v), using GPU UUID + mainboard UUID", tpmErr)
	gpuUUID := fetchGPUUUID()
	privKey, err := derivePrivateKey(mainboardUUID, gpuUUID)
	if err != nil {
		return NodeIdentity{}, err
	}

	return NodeIdentity{
		Address:       crypto.PubkeyToAddress(privKey.PublicKey).Hex(),
		MainboardUUID: mainboardUUID,
		GPUUID:        gpuUUID,
		privateKey:    privKey,
	}, nil
}

// Sign signs a message hash with the node's derived private key.
func (n *NodeIdentity) Sign(msgHash []byte) ([]byte, error) {
	return crypto.Sign(msgHash, n.privateKey)
}

// PrivateKeyHex returns the hex-encoded private key (use with caution).
func (n *NodeIdentity) PrivateKeyHex() string {
	return hex.EncodeToString(crypto.FromECDSA(n.privateKey))
}

// HasTPM returns true if the node identity was derived from fTPM.
func (n *NodeIdentity) HasTPM() bool {
	return n.TPMFingerprint != ""
}

func derivePrivateKey(part1, part2 string) (*ecdsa.PrivateKey, error) {
	secret := []byte(part1 + "|" + part2)
	salt := []byte("iann-node-v1")

	reader := hkdf.New(sha256.New, secret, salt, nil)
	privKeyBytes := make([]byte, 32)
	if _, err := io.ReadFull(reader, privKeyBytes); err != nil {
		return nil, err
	}

	return crypto.ToECDSA(privKeyBytes)
}
