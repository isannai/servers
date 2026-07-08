package setup

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
)

// TPMInfo holds TPM-derived identity data.
type TPMInfo struct {
	Fingerprint string // SHA256 hex of EK public key
	EKCertRaw   []byte // EK certificate DER bytes
	EKCertIssuer string // Issuer CN for structure verification
}

// ReadTPMInfo opens the TPM, reads the EK public key and certificate.
// Returns an error if TPM is not available.
func ReadTPMInfo() (*TPMInfo, error) {
	rwc, err := openTPM()
	if err != nil {
		return nil, fmt.Errorf("tpm: open: %w", err)
	}
	defer rwc.Close()

	ek, err := readEK(rwc)
	if err != nil {
		return nil, fmt.Errorf("tpm: read EK: %w", err)
	}
	defer ek.close()

	pubKeyDER, err := x509.MarshalPKIXPublicKey(ek.publicKey)
	if err != nil {
		return nil, fmt.Errorf("tpm: marshal EK public key: %w", err)
	}

	fp := sha256.Sum256(pubKeyDER)

	info := &TPMInfo{
		Fingerprint: hex.EncodeToString(fp[:]),
	}

	if ek.cert != nil {
		info.EKCertRaw = ek.cert.Raw
		info.EKCertIssuer = ek.cert.Issuer.CommonName
	}

	return info, nil
}

// SignWithTPM signs a nonce using a TPM-resident key.
// Returns the signature bytes.
func SignWithTPM(nonce []byte) ([]byte, error) {
	rwc, err := openTPM()
	if err != nil {
		return nil, fmt.Errorf("tpm: open: %w", err)
	}
	defer rwc.Close()

	return decryptWithEK(rwc, nonce, nil)
}

// ekHandle wraps EK key handle and optional certificate.
type ekHandle struct {
	publicKey interface{}
	cert      *x509.Certificate
	close     func()
}

// openTPM, readEK, decryptWithEK are implemented per-platform
// in tpm_windows.go and tpm_linux.go.
