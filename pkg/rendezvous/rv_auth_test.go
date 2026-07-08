package rendezvous

import (
	"crypto/ecdsa"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isannai/isann-servers/pkg/auth"
	"github.com/isannai/isann-servers/pkg/signal"
	"github.com/isannai/isann-servers/pkg/tunnel"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestAdmitRegister(t *testing.T) {
	issuerKey, _ := crypto.GenerateKey()
	issuerAddr := crypto.PubkeyToAddress(issuerKey.PublicKey)
	bearerKey, _ := crypto.GenerateKey()
	bearerAddr := crypto.PubkeyToAddress(bearerKey.PublicKey)
	nodeKey, _ := crypto.GenerateKey()
	nodeAddr := crypto.PubkeyToAddress(nodeKey.PublicKey)
	nodeID := "P:" + nodeAddr.Hex()

	srv := &Server{
		Mode: "protected",
		Issuers: map[string]IssuerPolicy{
			strings.ToLower(issuerAddr.Hex()): {Bind: "node"}, // node-bound
			strings.ToLower(bearerAddr.Hex()): {Bind: "none"}, // bearer
		},
	}

	// mint signs CredentialMessage(nodeAddr, issuedMs, expireMs) — node-bound.
	mint := func(issuedMs, expireMs int64, key *ecdsa.PrivateKey) string {
		sig, err := auth.SignMessage(tunnel.CredentialMessage(nodeAddr.Hex(), issuedMs, expireMs), key)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return sig
	}
	// mintBearer signs CredentialMessage("", issuedMs, expireMs) — bearer.
	mintBearer := func(issuedMs, expireMs int64, key *ecdsa.PrivateKey) string {
		sig, err := auth.SignMessage(tunnel.CredentialMessage("", issuedMs, expireMs), key)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return sig
	}
	now := time.Now().UnixMilli()
	future := now + int64(24*time.Hour/time.Millisecond)

	t.Run("valid", func(t *testing.T) {
		if err := srv.admitRegister(&tunnel.RendezvousMsg{ID: nodeID, CredSig: mint(now, future, issuerKey), CredIssuedMs: now, CredExpireMs: future}); err != nil {
			t.Fatalf("valid credential rejected: %v", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		if err := srv.admitRegister(&tunnel.RendezvousMsg{ID: nodeID}); err == nil {
			t.Fatal("missing credential accepted")
		}
	})

	t.Run("no_expiry_rejected", func(t *testing.T) {
		// expire=0 → a protected RV must reject (expiry is the sole lifetime).
		if err := srv.admitRegister(&tunnel.RendezvousMsg{ID: nodeID, CredSig: mint(now, 0, issuerKey), CredIssuedMs: now, CredExpireMs: 0}); err == nil {
			t.Fatal("credential with no expiry accepted")
		}
	})

	t.Run("unknown_issuer", func(t *testing.T) {
		otherKey, _ := crypto.GenerateKey()
		if err := srv.admitRegister(&tunnel.RendezvousMsg{ID: nodeID, CredSig: mint(now, future, otherKey), CredIssuedMs: now, CredExpireMs: future}); err == nil {
			t.Fatal("unknown issuer accepted")
		}
	})

	t.Run("expired", func(t *testing.T) {
		past := now - int64(time.Hour/time.Millisecond) // expired an hour ago
		if err := srv.admitRegister(&tunnel.RendezvousMsg{ID: nodeID, CredSig: mint(now, past, issuerKey), CredIssuedMs: now, CredExpireMs: past}); err == nil {
			t.Fatal("expired credential accepted")
		}
	})

	t.Run("node_bound", func(t *testing.T) {
		// A credential minted for nodeAddr can't be replayed for another node:
		// admitRegister rebuilds CredentialMessage(otherNodeAddr,...) and recovers
		// a different (wrong) address → not in issuers.
		otherNodeKey, _ := crypto.GenerateKey()
		otherID := "P:" + crypto.PubkeyToAddress(otherNodeKey.PublicKey).Hex()
		if err := srv.admitRegister(&tunnel.RendezvousMsg{ID: otherID, CredSig: mint(now, future, issuerKey), CredIssuedMs: now, CredExpireMs: future}); err == nil {
			t.Fatal("credential replayed for a different node accepted")
		}
	})

	t.Run("bearer_admits_any_node", func(t *testing.T) {
		// bind:none issuer — a bearer credential admits an arbitrary node id.
		k, _ := crypto.GenerateKey()
		anyID := "P:" + crypto.PubkeyToAddress(k.PublicKey).Hex()
		if err := srv.admitRegister(&tunnel.RendezvousMsg{ID: anyID, CredSig: mintBearer(now, future, bearerKey), CredIssuedMs: now, CredExpireMs: future}); err != nil {
			t.Fatalf("bearer credential rejected for arbitrary node: %v", err)
		}
	})

	t.Run("bearer_from_node_only_issuer_rejected", func(t *testing.T) {
		// the node-bound issuer (bind:node) must NOT pass a bearer-shaped cred.
		if err := srv.admitRegister(&tunnel.RendezvousMsg{ID: nodeID, CredSig: mintBearer(now, future, issuerKey), CredIssuedMs: now, CredExpireMs: future}); err == nil {
			t.Fatal("bearer credential from a node-only issuer was accepted")
		}
	})

	t.Run("bound_from_bearer_only_issuer_rejected", func(t *testing.T) {
		// the bearer issuer (bind:none) must NOT pass a node-bound cred.
		if err := srv.admitRegister(&tunnel.RendezvousMsg{ID: nodeID, CredSig: mint(now, future, bearerKey), CredIssuedMs: now, CredExpireMs: future}); err == nil {
			t.Fatal("node-bound credential from a bearer-only issuer was accepted")
		}
	})
}

func TestLoadRVAuth(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "rendezvous.json")
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAuth := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("parse_and_bind_default", func(t *testing.T) {
		writeAuth(`{"mode":"protected","issuers":[{"address":"0xABC","bind":"any"},{"address":"0xDEF"}]}`)
		mode, issuers, err := LoadRVAuth(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if mode != "protected" {
			t.Fatalf("mode=%q", mode)
		}
		if issuers["0xabc"].Bind != "any" {
			t.Fatalf("bind=%q want any", issuers["0xabc"].Bind)
		}
		if issuers["0xdef"].Bind != "node" {
			t.Fatalf("bind=%q want node (default)", issuers["0xdef"].Bind)
		}
	})

	t.Run("invalid_bind_errors", func(t *testing.T) {
		writeAuth(`{"mode":"protected","issuers":[{"address":"0xABC","bind":"bogus"}]}`)
		if _, _, err := LoadRVAuth(cfgPath); err == nil {
			t.Fatal("invalid bind should error")
		}
	})

	t.Run("open_alias_to_public", func(t *testing.T) {
		writeAuth(`{"mode":"open"}`)
		mode, _, err := LoadRVAuth(cfgPath)
		if err != nil || mode != "public" {
			t.Fatalf("open→public: mode=%q err=%v", mode, err)
		}
	})

	t.Run("missing_file_is_public", func(t *testing.T) {
		mode, issuers, err := LoadRVAuth(filepath.Join(dir, "nope", "rendezvous.json"))
		if err != nil {
			t.Fatal(err)
		}
		if mode != "public" || len(issuers) != 0 {
			t.Fatalf("missing: mode=%q issuers=%d", mode, len(issuers))
		}
	})

	t.Run("protected_without_issuers_errors", func(t *testing.T) {
		writeAuth(`{"mode":"protected"}`)
		if _, _, err := LoadRVAuth(cfgPath); err == nil {
			t.Fatal("protected with no issuers should error")
		}
	})
}

// TestApplyRegisterProtectedGate drives the FULL register path end-to-end
// (in-process): a node signs a real RegisterDigest + an issuer signs a real
// credential, then applyRegister runs the actual signature verify + admission
// gate and we assert the proxies entry appears (admitted) or not (rejected).
// This is the "does it actually work" proof without a live RV.
func TestApplyRegisterProtectedGate(t *testing.T) {
	issuerKey, _ := crypto.GenerateKey()
	issuerAddr := crypto.PubkeyToAddress(issuerKey.PublicKey)
	nodeKeyPair, _ := crypto.GenerateKey()
	nodeAddr := crypto.PubkeyToAddress(nodeKeyPair.PublicKey)
	nodeID := "P:" + nodeAddr.Hex()

	newSrv := func() *Server {
		return &Server{
			Mode:    "protected",
			Issuers: map[string]IssuerPolicy{strings.ToLower(issuerAddr.Hex()): {Bind: "node"}},
			proxies: map[string]*ProxyInfo{},
		}
	}

	// buildReg signs a real FullSync register frame (node's RegisterDigest sig)
	// and attaches the given credential.
	buildReg := func(credSig string, credIssued, credExpire int64) *tunnel.RendezvousMsg {
		ts := time.Now().UnixMilli()
		digest := tunnel.RegisterDigest(nodeID, "", "", "", "", "", ts)
		regSig, err := crypto.Sign(digest, nodeKeyPair) // V in {0,1}, matches SigToPub
		if err != nil {
			t.Fatal(err)
		}
		return &tunnel.RendezvousMsg{
			V: 1, Type: signal.TypeRegister, Role: "provider", ID: nodeID,
			FullSync: true, Signature: regSig, TimestampMs: ts,
			CredSig: credSig, CredIssuedMs: credIssued, CredExpireMs: credExpire,
		}
	}

	// newCC gives a controlConn over an in-memory pipe; drain the far end so
	// reject-path error frames don't block applyRegister.
	newCC := func() *controlConn {
		c1, c2 := net.Pipe()
		go io.Copy(io.Discard, c2)
		return &controlConn{conn: c1, nodeID: nodeID, role: "provider", connectedAt: time.Now()}
	}

	now := time.Now().UnixMilli()
	future := now + int64(24*time.Hour/time.Millisecond)
	mintCred := func(issued, expire int64, key *ecdsa.PrivateKey) string {
		sig, err := auth.SignMessage(tunnel.CredentialMessage(nodeAddr.Hex(), issued, expire), key)
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	admitted := func(srv *Server) bool {
		_, ok := srv.proxies[nodeKey(nodeID)]
		return ok
	}

	t.Run("valid_credential_registers", func(t *testing.T) {
		srv := newSrv()
		srv.applyRegister(newCC(), buildReg(mintCred(now, future, issuerKey), now, future))
		if !admitted(srv) {
			t.Fatal("valid node was NOT admitted")
		}
	})

	t.Run("missing_credential_rejected", func(t *testing.T) {
		srv := newSrv()
		srv.applyRegister(newCC(), buildReg("", 0, 0))
		if admitted(srv) {
			t.Fatal("node without admission credential WAS admitted")
		}
	})

	t.Run("expired_credential_rejected", func(t *testing.T) {
		past := now - int64(time.Hour/time.Millisecond)
		srv := newSrv()
		srv.applyRegister(newCC(), buildReg(mintCred(now, past, issuerKey), now, past))
		if admitted(srv) {
			t.Fatal("node with expired credential WAS admitted")
		}
	})

	t.Run("unknown_issuer_rejected", func(t *testing.T) {
		otherKey, _ := crypto.GenerateKey()
		srv := newSrv()
		srv.applyRegister(newCC(), buildReg(mintCred(now, future, otherKey), now, future))
		if admitted(srv) {
			t.Fatal("node with unknown-issuer credential WAS admitted")
		}
	})

	t.Run("public_mode_admits_without_credential", func(t *testing.T) {
		srv := newSrv()
		srv.Mode = "public"
		srv.applyRegister(newCC(), buildReg("", 0, 0))
		if !admitted(srv) {
			t.Fatal("public mode rejected a node that has no credential")
		}
	})
}
