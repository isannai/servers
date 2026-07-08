//go:build windows

package setup

import (
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"io"

	"github.com/google/go-tpm-tools/client"
	"github.com/google/go-tpm/legacy/tpm2"
)

func openTPM() (io.ReadWriteCloser, error) {
	return tpm2.OpenTPM()
}

func readEK(rwc io.ReadWriteCloser) (*ekHandle, error) {
	ek, err := client.EndorsementKeyRSA(rwc)
	if err != nil {
		return nil, fmt.Errorf("EndorsementKeyRSA: %w", err)
	}

	return &ekHandle{
		publicKey: ek.PublicKey(),
		cert:      ek.Cert(),
		close:     func() { ek.Close() },
	}, nil
}

// decryptWithEK signs the nonce using a transient TPM signing key under SRK.
// SRK doesn't require owner auth (unlike AK which needs owner hierarchy).
// The second parameter is unused (kept for interface compatibility).
func decryptWithEK(rwc io.ReadWriteCloser, nonce []byte, _ []byte) ([]byte, error) {
	tmpl := tpm2.Public{
		Type:    tpm2.AlgRSA,
		NameAlg: tpm2.AlgSHA256,
		Attributes: tpm2.FlagFixedTPM | tpm2.FlagFixedParent |
			tpm2.FlagSensitiveDataOrigin | tpm2.FlagUserWithAuth |
			tpm2.FlagSign,
		RSAParameters: &tpm2.RSAParams{
			Sign: &tpm2.SigScheme{
				Alg:  tpm2.AlgRSASSA,
				Hash: tpm2.AlgSHA256,
			},
			KeyBits: 2048,
		},
	}

	keyHandle, _, err := tpm2.CreatePrimary(rwc, tpm2.HandleOwner, tpm2.PCRSelection{}, "", "", tmpl)
	if err != nil {
		return nil, fmt.Errorf("CreatePrimary: %w", err)
	}
	defer tpm2.FlushContext(rwc, keyHandle)

	hash := sha256.Sum256(nonce)
	sig, err := tpm2.Sign(rwc, keyHandle, "", hash[:], nil, &tpm2.SigScheme{
		Alg:  tpm2.AlgRSASSA,
		Hash: tpm2.AlgSHA256,
	})
	if err != nil {
		return nil, fmt.Errorf("TPM Sign: %w", err)
	}

	return sig.RSA.Signature, nil
}

// ReadAKPublicHash returns the EK fingerprint (used as identity binding for challenge).
func ReadAKPublicHash() ([]byte, error) {
	rwc, err := openTPM()
	if err != nil {
		return nil, err
	}
	defer rwc.Close()

	ek, err := client.EndorsementKeyRSA(rwc)
	if err != nil {
		return nil, fmt.Errorf("EndorsementKeyRSA: %w", err)
	}
	defer ek.Close()

	pubDER, err := x509.MarshalPKIXPublicKey(ek.PublicKey())
	if err != nil {
		return nil, fmt.Errorf("MarshalPKIXPublicKey: %w", err)
	}
	hash := sha256.Sum256(pubDER)
	return hash[:], nil
}
