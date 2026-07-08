package auth

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// TestSignRecoverRoundtrip is the regression net for Phase 2 §6 cross-node
// auth: SignMessage and RecoverAddress must be exact inverses (same eth
// prefix, same keccak, matching V offset). If they ever diverge, every
// remote admin request silently 403s — so this guards the single most
// load-bearing pair in the cross-node path.
func TestSignRecoverRoundtrip(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	want := crypto.PubkeyToAddress(key.PublicKey).Hex()

	canonical := BuildCanonical(
		"POST",
		"/internal/api/docker/start/llama",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", // sha256("")
		"P:0x8fF1234567890abcdef1234567890abcdef123456",
		"0123456789abcdef0123456789abcdef",
		"1748563200",
	)

	sig, err := SignMessage(canonical, key)
	if err != nil {
		t.Fatalf("SignMessage: %v", err)
	}

	got, err := RecoverAddress(canonical, sig)
	if err != nil {
		t.Fatalf("RecoverAddress: %v", err)
	}
	if !strings.EqualFold(got, want) {
		t.Fatalf("roundtrip mismatch: got %s want %s", got, want)
	}

	// 0x-prefixed signature must also recover (RecoverAddress strips 0x).
	if got2, err := RecoverAddress(canonical, "0x"+sig); err != nil || !strings.EqualFold(got2, want) {
		t.Fatalf("0x-prefixed recover: got %s err %v want %s", got2, err, want)
	}
}

// TestRecoverRejectsTamper confirms a one-byte change to the canonical
// (e.g. a swapped path or nodeID) recovers a DIFFERENT address — the basis
// of cross-endpoint / cross-node replay protection.
func TestRecoverRejectsTamper(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	addr := crypto.PubkeyToAddress(key.PublicKey).Hex()

	signed := BuildCanonical("POST", "/internal/api/docker/start/llama", "abc", "P:0xAAA", "n1", "1748563200")
	sig, err := SignMessage(signed, key)
	if err != nil {
		t.Fatalf("SignMessage: %v", err)
	}

	// Verifier reconstructs with a different target node — must NOT match addr.
	tampered := BuildCanonical("POST", "/internal/api/docker/start/llama", "abc", "P:0xBBB", "n1", "1748563200")
	got, err := RecoverAddress(tampered, sig)
	if err != nil {
		t.Fatalf("RecoverAddress: %v", err)
	}
	if strings.EqualFold(got, addr) {
		t.Fatalf("tampered canonical recovered the signer address %s — replay protection broken", addr)
	}
}

// TestBuildCanonicalDeterministic — same inputs always yield the same bytes
// (signer and verifier must agree exactly).
func TestBuildCanonicalDeterministic(t *testing.T) {
	a := BuildCanonical("GET", "/internal/api/docker/ps", "h", "P:0x1", "n", "1")
	b := BuildCanonical("GET", "/internal/api/docker/ps", "h", "P:0x1", "n", "1")
	if a != b {
		t.Fatalf("non-deterministic: %q vs %q", a, b)
	}
	if want := "GET\n/internal/api/docker/ps\nh\nP:0x1\nn\n1"; a != want {
		t.Fatalf("canonical format: got %q want %q", a, want)
	}
}
