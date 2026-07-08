package market

// server_test.go — end-to-end coverage of the market backend over a real
// httptest server + SQLite store, driving the exact canonical-signature flow a
// CLI/wallet would. This is the automated half of the verification harness;
// the manual/curl half lives in docs/TODO/market/test/.
//
// Run:  go test ./pkg/market/ -run TestMarket -v

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/isannai/isann-servers/pkg/auth"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// nonceCtr guarantees unique nonces even under Windows' coarse clock, where
// back-to-back time.Now() values can be identical.
var nonceCtr int64

// testEnv spins a market server on an ephemeral SQLite DB.
type testEnv struct {
	t   *testing.T
	srv *httptest.Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := Config{}
	cfg.applyDefaults()
	cfg.DB.DSN = filepath.Join(t.TempDir(), "market_test.db")
	store, err := NewStore("sqlite", cfg.DB.DSN)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ts := httptest.NewServer(NewServer(cfg, store).mux)
	t.Cleanup(ts.Close)
	return &testEnv{t: t, srv: ts}
}

// signedDo issues a request signed with pk over the canonical the server
// reconstructs (method + RequestURI + sha256(body) + target + nonce + ts).
func (e *testEnv) signedDo(method, path string, body []byte, pk *ecdsa.PrivateKey, target string) (*http.Response, []byte) {
	e.t.Helper()
	nonce := "n-" + strconv.FormatInt(atomic.AddInt64(&nonceCtr, 1), 10)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := sha256.Sum256(body)
	canonical := auth.BuildCanonical(method, path, hex.EncodeToString(h[:]), strings.ToLower(target), nonce, ts)
	sig, err := auth.SignMessage(canonical, pk)
	if err != nil {
		e.t.Fatalf("sign: %v", err)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "ISANN "+sig)
	req.Header.Set("X-ISANN-Nonce", nonce)
	req.Header.Set("X-ISANN-Timestamp", ts)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, out
}

func (e *testEnv) get(path string) (*http.Response, []byte) {
	e.t.Helper()
	resp, err := http.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatalf("GET %s: %v", path, err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, out
}

func newKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	k, err := ethcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k, strings.ToLower(ethcrypto.PubkeyToAddress(k.PublicKey).Hex())
}

func mkRecipe(name, version, summary string) string {
	return fmt.Sprintf("#pragma ISANN 0.1.20\n# name: %s\n# version: %s\n# summary: %s\n# license: MIT\n# tags: art, test\n\npreset set --name %s --engine llama temperature=1.2;\n",
		name, version, summary, name)
}

// TestMarketLifecycle drives the full author lifecycle end to end.
func TestMarketLifecycle(t *testing.T) {
	e := newTestEnv(t)
	alice, aliceAddr := newKey(t)

	base := "/v1/assets/preset/" + aliceAddr + "/creative"
	pushBody, _ := json.Marshal(map[string]string{"recipe": mkRecipe("creative", "1.0", "crisp creative preset")})

	// --- push (private by default) ---
	resp, out := e.signedDo(http.MethodPost, base, pushBody, alice, aliceAddr)
	if resp.StatusCode != 201 {
		t.Fatalf("push: want 201, got %d — %s", resp.StatusCode, out)
	}
	var asset Asset
	json.Unmarshal(out, &asset)
	if asset.Visibility != "private" || asset.Version != "1.0" {
		t.Fatalf("push asset unexpected: %+v", asset)
	}

	// --- private asset is hidden from anonymous browse and meta ---
	if resp, _ := e.get("/v1/assets"); mustCount(t, e) != 0 {
		_ = resp
		t.Fatalf("private asset leaked into public list")
	}
	if resp, _ := e.get(base); resp.StatusCode != 404 {
		t.Fatalf("anon meta of private: want 404, got %d", resp.StatusCode)
	}

	// --- author can read own private asset (signed GET) ---
	if resp, _ := e.signedDo(http.MethodGet, base, nil, alice, aliceAddr); resp.StatusCode != 200 {
		t.Fatalf("author private meta: want 200, got %d", resp.StatusCode)
	}

	// --- a different wallet cannot push under alice's path ---
	mallory, _ := newKey(t)
	if resp, _ := e.signedDo(http.MethodPost, base, pushBody, mallory, aliceAddr); resp.StatusCode != 401 {
		t.Fatalf("cross-author push: want 401, got %d", resp.StatusCode)
	}

	// --- duplicate version is immutable → 409 ---
	if resp, _ := e.signedDo(http.MethodPost, base, pushBody, alice, aliceAddr); resp.StatusCode != 409 {
		t.Fatalf("dup version: want 409, got %d", resp.StatusCode)
	}

	// --- publish (visibility=public), then it shows in browse ---
	visBody, _ := json.Marshal(map[string]string{"visibility": "public"})
	if resp, _ := e.signedDo(http.MethodPatch, base, visBody, alice, aliceAddr); resp.StatusCode != 200 {
		t.Fatalf("publish: want 200, got %d", resp.StatusCode)
	}
	if n := mustCount(t, e); n != 1 {
		t.Fatalf("public list count: want 1, got %d", n)
	}
	// publish again = idempotent skip
	if resp, body := e.signedDo(http.MethodPatch, base, visBody, alice, aliceAddr); resp.StatusCode != 200 || !strings.Contains(string(body), "skip") {
		t.Fatalf("publish idempotency: got %d %s", resp.StatusCode, body)
	}

	// --- push a newer version; latest pointer moves ---
	push2, _ := json.Marshal(map[string]string{"recipe": mkRecipe("creative", "1.1", "v1.1 tweaks")})
	e.signedDo(http.MethodPost, base, push2, alice, aliceAddr)
	if resp, out := e.get(base); resp.StatusCode != 200 {
		t.Fatalf("meta after v1.1: %d", resp.StatusCode)
	} else {
		var a Asset
		json.Unmarshal(out, &a)
		if a.Version != "1.1" {
			t.Fatalf("latest pointer: want 1.1, got %s", a.Version)
		}
	}

	// --- install (public, anonymous) returns recipe body + sha header ---
	resp, out = e.get(base + "/install")
	if resp.StatusCode != 200 {
		t.Fatalf("install: want 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(out), "#pragma ISANN") {
		t.Fatalf("install body not a recipe: %s", out)
	}
	sum := sha256.Sum256(out)
	if got := resp.Header.Get("X-ISANN-Sha256"); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("install sha mismatch: header=%s", got)
	}
	// pin an old version
	if resp, out := e.get(base + "/install?version=1.0"); resp.StatusCode != 200 || !strings.Contains(string(out), "# version: 1.0") {
		t.Fatalf("install pinned 1.0: %d — %s", resp.StatusCode, out)
	}

	// --- versions list has both, newest first ---
	if resp, out := e.get(base + "/versions"); resp.StatusCode != 200 {
		t.Fatalf("versions: %d", resp.StatusCode)
	} else {
		var vr struct{ Versions []Version }
		json.Unmarshal(out, &vr)
		if len(vr.Versions) != 2 || vr.Versions[0].Version != "1.1" {
			t.Fatalf("versions order: %+v", vr.Versions)
		}
	}

	// --- price: make it paid, then free (idempotent) ---
	priceBody, _ := json.Marshal(map[string]string{"price": "5", "token": "USDC"})
	if resp, _ := e.signedDo(http.MethodPatch, base+"/price", priceBody, alice, aliceAddr); resp.StatusCode != 200 {
		t.Fatalf("price set: %d", resp.StatusCode)
	}
	freeBody, _ := json.Marshal(map[string]bool{"free": true})
	if resp, _ := e.signedDo(http.MethodPatch, base+"/price", freeBody, alice, aliceAddr); resp.StatusCode != 200 {
		t.Fatalf("price free: %d", resp.StatusCode)
	}

	// --- mine lists alice's assets (private included) ---
	if resp, out := e.signedDo(http.MethodGet, "/v1/assets/mine", nil, alice, "mine"); resp.StatusCode != 200 {
		t.Fatalf("mine: %d — %s", resp.StatusCode, out)
	} else {
		var mr struct{ Total int }
		json.Unmarshal(out, &mr)
		if mr.Total != 1 {
			t.Fatalf("mine total: want 1, got %d", mr.Total)
		}
	}

	// --- unpush (delete), then idempotent skip ---
	if resp, _ := e.signedDo(http.MethodDelete, base, nil, alice, aliceAddr); resp.StatusCode != 200 {
		t.Fatalf("unpush: %d", resp.StatusCode)
	}
	if resp, body := e.signedDo(http.MethodDelete, base, nil, alice, aliceAddr); resp.StatusCode != 200 || !strings.Contains(string(body), "skip") {
		t.Fatalf("unpush idempotency: %d %s", resp.StatusCode, body)
	}
	if n := mustCount(t, e); n != 0 {
		t.Fatalf("after unpush count: want 0, got %d", n)
	}
}

// TestMarketReplay asserts a replayed nonce is rejected.
func TestMarketReplay(t *testing.T) {
	e := newTestEnv(t)
	alice, aliceAddr := newKey(t)
	base := "/v1/assets/preset/" + aliceAddr + "/x"
	body, _ := json.Marshal(map[string]string{"recipe": mkRecipe("x", "1.0", "s")})

	// Manually reuse a nonce across two requests.
	nonce := "fixed-nonce"
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := sha256.Sum256(body)
	canonical := auth.BuildCanonical(http.MethodPost, base, hex.EncodeToString(h[:]), aliceAddr, nonce, ts)
	sig, _ := auth.SignMessage(canonical, alice)
	do := func() int {
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+base, bytes.NewReader(body))
		req.Header.Set("Authorization", "ISANN "+sig)
		req.Header.Set("X-ISANN-Nonce", nonce)
		req.Header.Set("X-ISANN-Timestamp", ts)
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := do(); c != 201 {
		t.Fatalf("first push: want 201, got %d", c)
	}
	if c := do(); c == 201 {
		t.Fatalf("replay accepted (want rejection), got %d", c)
	}
}

func mustCount(t *testing.T, e *testEnv) int {
	t.Helper()
	_, out := e.get("/v1/assets")
	var r struct{ Total int }
	json.Unmarshal(out, &r)
	return r.Total
}
