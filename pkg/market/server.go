package market

// server.go — the market HTTP server. Public read is CORS-open and
// unauthenticated; write (and private read) is author-signed (auth_write.go).
// Routing mirrors pkg/gate/server.go: a ServeMux with prefix handlers that
// split the path themselves. Endpoints follow market-server.md §10.5.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/isannai/isann-servers/pkg/certgen"
	"github.com/isannai/isann-servers/pkg/setup"
)

// Server is the market registry HTTP server.
type Server struct {
	cfg    Config
	store  Store
	mux    *http.ServeMux
	nonces *nonceCache
}

// NewServer builds a market server over the given store.
func NewServer(cfg Config, store Store) *Server {
	s := &Server{cfg: cfg, store: store, mux: http.NewServeMux(), nonces: newNonceCache()}
	s.routes()
	return s
}

// Run starts listening (HTTP or HTTPS per config).
func (s *Server) Run() error {
	if s.cfg.DevInsecureSkipAuth {
		log.Printf("[market] ⚠ DEV_INSECURE_SKIP_AUTH is ON — write signatures are DISABLED. Local dev only.")
	}
	handler := s.corsMiddleware(s.mux)
	if s.cfg.TLS.Enabled {
		// Auto-generate a self-signed cert on first boot when none is provided,
		// so `docker compose up` works out of the box. Production mounts real certs.
		if err := certgen.EnsureSelfSigned(s.cfg.TLS.Cert, s.cfg.TLS.Key); err != nil {
			return err
		}
		log.Printf("[market] listening on %s (HTTPS)", s.cfg.Addr)
		return http.ListenAndServeTLS(s.cfg.Addr, s.cfg.TLS.Cert, s.cfg.TLS.Key, handler)
	}
	log.Printf("[market] listening on %s (HTTP)", s.cfg.Addr)
	return http.ListenAndServe(s.cfg.Addr, handler)
}

// market is a pure JSON API server — there is no static frontend to serve.
// The public site (isann-registry Explore/Repositories) is a separate app that
// consumes these endpoints over CORS-open public read (market-server.md §7).
func (s *Server) routes() {
	s.mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "service": "market", "version": setup.MarketVersion})
	})
	// /v1/assets (exact) = list/browse; /v1/assets/... = detail/write/mine.
	s.mux.HandleFunc("/v1/assets", s.handleAssetsList)
	s.mux.HandleFunc("/v1/assets/", s.handleAssetsPath)
	// Phase-3 paid payload fetch — stubbed until x402/8183 land.
	s.mux.HandleFunc("/v1/blob/", s.handleBlob)
}

// --- GET /v1/assets — browse / search --------------------------------------

func (s *Server) handleAssetsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, errBody("method_not_allowed", "GET only"))
		return
	}
	q := r.URL.Query()
	f := ListFilter{
		Type:   q.Get("type"),
		Author: q.Get("author"),
		Query:  q.Get("q"),
		Sort:   q.Get("sort"),
		Page:   queryInt(q.Get("page"), 1),
		Limit:  queryInt(q.Get("limit"), 20),
	}
	if p := q.Get("paid"); p != "" {
		b := p == "true" || p == "1"
		f.Paid = &b
	}
	if f.Type != "" && !ValidTypes[f.Type] {
		writeJSON(w, 400, errBody("bad_request", "unknown type: "+f.Type))
		return
	}
	items, total, err := s.store.ListAssets(r.Context(), f)
	if err != nil {
		writeJSON(w, 500, errBody("internal", err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "page": f.Page, "limit": clampLimit(f.Limit), "total": total})
}

// --- /v1/assets/... — detail, write, mine ----------------------------------

func (s *Server) handleAssetsPath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/assets/")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		writeJSON(w, 404, errBody("not_found", "asset path required"))
		return
	}
	segs := strings.Split(rest, "/")

	// /v1/assets/mine
	if len(segs) == 1 && segs[0] == "mine" {
		s.handleMine(w, r)
		return
	}

	// /v1/assets/{type}/{author}/{name}[/{sub}]
	if len(segs) < 3 || len(segs) > 4 {
		writeJSON(w, 404, errBody("not_found", "expected /v1/assets/{type}/{author}/{name}[/versions|install|price]"))
		return
	}
	typ, author, name := segs[0], strings.ToLower(segs[1]), segs[2]
	if !ValidTypes[typ] {
		writeJSON(w, 400, errBody("bad_request", "unknown type: "+typ))
		return
	}
	sub := ""
	if len(segs) == 4 {
		sub = segs[3]
	}

	switch sub {
	case "":
		switch r.Method {
		case http.MethodGet:
			s.handleGetMeta(w, r, typ, author, name)
		case http.MethodPost:
			s.handlePush(w, r, typ, author, name)
		case http.MethodPatch:
			s.handleVisibility(w, r, typ, author, name)
		case http.MethodDelete:
			s.handleUnpush(w, r, typ, author, name)
		default:
			writeJSON(w, 405, errBody("method_not_allowed", ""))
		}
	case "versions":
		if r.Method != http.MethodGet {
			writeJSON(w, 405, errBody("method_not_allowed", "GET only"))
			return
		}
		s.handleVersions(w, r, typ, author, name)
	case "install":
		if r.Method != http.MethodGet {
			writeJSON(w, 405, errBody("method_not_allowed", "GET only"))
			return
		}
		s.handleInstall(w, r, typ, author, name)
	case "price":
		if r.Method != http.MethodPatch {
			writeJSON(w, 405, errBody("method_not_allowed", "PATCH only"))
			return
		}
		s.handlePrice(w, r, typ, author, name)
	case "meta":
		if r.Method != http.MethodPatch {
			writeJSON(w, 405, errBody("method_not_allowed", "PATCH only"))
			return
		}
		s.handleMeta(w, r, typ, author, name)
	default:
		writeJSON(w, 404, errBody("not_found", "unknown subresource: "+sub))
	}
}

// GET meta — public asset open; private requires author signature (else 404 hide).
func (s *Server) handleGetMeta(w http.ResponseWriter, r *http.Request, typ, author, name string) {
	version := r.URL.Query().Get("version")
	a, versions, err := s.store.GetAsset(r.Context(), typ, author, name, version)
	if err != nil {
		writeJSON(w, 500, errBody("internal", err.Error()))
		return
	}
	if a == nil {
		writeJSON(w, 404, errBody("not_found", "asset not found"))
		return
	}
	if a.Visibility == "private" && !s.privateReadOK(w, r, author) {
		return // response already written (404 hide)
	}
	out := struct {
		Asset
		Versions []Version `json:"versions"`
	}{Asset: *a, Versions: versions}
	writeJSON(w, 200, out)
}

func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request, typ, author, name string) {
	versions, err := s.store.ListVersions(r.Context(), typ, author, name)
	if err != nil {
		writeJSON(w, 500, errBody("internal", err.Error()))
		return
	}
	if versions == nil {
		writeJSON(w, 404, errBody("not_found", "asset not found"))
		return
	}
	writeJSON(w, 200, map[string]any{"versions": versions})
}

// GET install — the install.ian body. Free (recipes are never metered — §8).
// public = anonymous; private = author signature.
func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request, typ, author, name string) {
	version := r.URL.Query().Get("version")
	body, sha, visibility, found, err := s.store.GetRecipe(r.Context(), typ, author, name, version)
	if err != nil {
		writeJSON(w, 500, errBody("internal", err.Error()))
		return
	}
	if !found {
		writeJSON(w, 404, errBody("not_found", "asset not found"))
		return
	}
	if visibility == "private" && !s.privateReadOK(w, r, author) {
		return
	}
	_ = s.store.IncrDownloads(r.Context(), typ, author, name)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-ISANN-Sha256", sha)
	w.WriteHeader(200)
	io.WriteString(w, body)
}

// POST push — upload a signed recipe. Metadata is parsed from the recipe (single
// source). New assets default to private. Duplicate version = 409 (immutable).
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request, typ, author, name string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MiB cap — recipes are KB
	if err != nil {
		writeJSON(w, 400, errBody("bad_request", "read body"))
		return
	}
	signer := s.verifyWrite(w, r, body, author)
	if signer == "" {
		return
	}
	if signer != author {
		writeJSON(w, 401, errBody("unauthorized", "recovered signer does not match path author"))
		return
	}
	var req struct {
		Recipe string `json:"recipe"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Recipe == "" {
		writeJSON(w, 400, errBody("bad_request", "body must be {\"recipe\": \"...\"}"))
		return
	}
	meta, err := ParseRecipeMeta(req.Recipe)
	if err != nil {
		writeJSON(w, 400, errBody("bad_request", err.Error()))
		return
	}
	if meta.Name != name {
		writeJSON(w, 400, errBody("bad_request", "recipe name "+meta.Name+" != path name "+name))
		return
	}
	// If the recipe declares an author, it must match the signer too.
	if meta.Author != "" && !strings.EqualFold(meta.Author, author) {
		writeJSON(w, 400, errBody("bad_request", "recipe author does not match signer"))
		return
	}
	sum := sha256.Sum256([]byte(req.Recipe))
	in := PushInput{
		Type: typ, Author: author, Meta: meta,
		RecipeBody: req.Recipe, SHA256: hex.EncodeToString(sum[:]),
	}
	a, err := s.store.PutAsset(r.Context(), in)
	if err == ErrVersionExists {
		writeJSON(w, 409, errBody("version_exists", "version "+meta.Version+" already published (immutable)"))
		return
	}
	if err != nil {
		writeJSON(w, 500, errBody("internal", err.Error()))
		return
	}
	writeJSON(w, 201, a)
}

// PATCH visibility — publish/unpublish (explicit set, idempotent).
func (s *Server) handleVisibility(w http.ResponseWriter, r *http.Request, typ, author, name string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if s.verifyWriteAuthor(w, r, body, author) == "" {
		return
	}
	var req struct {
		Visibility string `json:"visibility"`
	}
	if json.Unmarshal(body, &req) != nil || (req.Visibility != "public" && req.Visibility != "private") {
		writeJSON(w, 400, errBody("bad_request", "visibility must be 'public' or 'private'"))
		return
	}
	changed, err := s.store.SetVisibility(r.Context(), typ, author, name, req.Visibility)
	s.writeMutation(w, r, typ, author, name, changed, err)
}

// PATCH price — set the pricing axis (independent of visibility).
func (s *Server) handlePrice(w http.ResponseWriter, r *http.Request, typ, author, name string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if s.verifyWriteAuthor(w, r, body, author) == "" {
		return
	}
	var req struct {
		Price string `json:"price"`
		Token string `json:"token"`
		Free  bool   `json:"free"`
	}
	if json.Unmarshal(body, &req) != nil {
		writeJSON(w, 400, errBody("bad_request", "invalid JSON"))
		return
	}
	var price string
	var token *string
	switch {
	case req.Free || req.Price == "0":
		price = "0"
		token = nil
	case req.Price != "":
		if req.Token == "" {
			writeJSON(w, 400, errBody("bad_request", "token required when price > 0"))
			return
		}
		// NOTE: paid listings require ERC-8004 eligibility (market-eligibility.md).
		// The on-chain check is Phase 3 — for v1 we accept the pricing but the
		// paid *payload* fetch (/v1/blob) stays stubbed.
		price = req.Price
		token = &req.Token
	default:
		writeJSON(w, 400, errBody("bad_request", "provide price+token, or free:true"))
		return
	}
	changed, err := s.store.SetPrice(r.Context(), typ, author, name, price, token)
	s.writeMutation(w, r, typ, author, name, changed, err)
}

// PATCH meta — edit mutable discovery metadata (tags, summary) without a
// version bump. Content/versions stay immutable; a later push re-derives these.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request, typ, author, name string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if s.verifyWriteAuthor(w, r, body, author) == "" {
		return
	}
	// tags accepted as a comma string OR a JSON array; summary as a string.
	// Pointers distinguish "omitted" (leave unchanged) from set.
	var req struct {
		Tags    *json.RawMessage `json:"tags"`
		Summary *string          `json:"summary"`
	}
	if json.Unmarshal(body, &req) != nil {
		writeJSON(w, 400, errBody("bad_request", "invalid JSON"))
		return
	}
	var tags *string
	if req.Tags != nil {
		norm := normalizeTags(*req.Tags)
		tags = &norm
	}
	if tags == nil && req.Summary == nil {
		writeJSON(w, 400, errBody("bad_request", "provide tags and/or summary"))
		return
	}
	changed, err := s.store.SetMeta(r.Context(), typ, author, name, tags, req.Summary)
	s.writeMutation(w, r, typ, author, name, changed, err)
}

// normalizeTags accepts a JSON string ("a,b") or array (["a","b"]) and returns
// a cleaned comma-joined form (trimmed, empties dropped).
func normalizeTags(raw json.RawMessage) string {
	var arr []string
	if json.Unmarshal(raw, &arr) != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
		arr = strings.Split(s, ",")
	}
	out := make([]string, 0, len(arr))
	for _, t := range arr {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, ",")
}

// DELETE unpush — remove the asset entirely (author only, idempotent skip).
func (s *Server) handleUnpush(w http.ResponseWriter, r *http.Request, typ, author, name string) {
	if s.verifyWriteAuthor(w, r, nil, author) == "" {
		return
	}
	existed, err := s.store.DeleteAsset(r.Context(), typ, author, name)
	if err != nil {
		writeJSON(w, 500, errBody("internal", err.Error()))
		return
	}
	if !existed {
		writeJSON(w, 200, map[string]string{"status": "skip", "message": "nothing to delete"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// GET /v1/assets/mine — the signer's own assets (all types, private included).
func (s *Server) handleMine(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, errBody("method_not_allowed", "GET only"))
		return
	}
	// No path author — the recovered signer IS the author; target = "mine".
	signer := s.verifyWrite(w, r, nil, "mine")
	if signer == "" {
		return
	}
	// Dev-skip has no signature to recover an author from, so take it from
	// ?author= (plain-curl testing).
	if s.cfg.DevInsecureSkipAuth {
		if a := r.URL.Query().Get("author"); a != "" {
			signer = strings.ToLower(a)
		}
	}
	q := r.URL.Query()
	f := ListFilter{Type: q.Get("type"), Page: queryInt(q.Get("page"), 1), Limit: queryInt(q.Get("limit"), 20)}
	items, total, err := s.store.ListMine(r.Context(), signer, f)
	if err != nil {
		writeJSON(w, 500, errBody("internal", err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "page": f.Page, "limit": clampLimit(f.Limit), "total": total})
}

// handleBlob — Phase-3 paid payload fetch (x402/8183). Stub for v1.
func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, errBody("not_implemented", "paid payload fetch is Phase 3 (x402/8183)"))
}

// --- shared helpers ---------------------------------------------------------

// verifyWriteAuthor verifies the signature AND that the signer == path author,
// the common gate for mutations. Returns the signer, or "" (response written).
func (s *Server) verifyWriteAuthor(w http.ResponseWriter, r *http.Request, body []byte, author string) string {
	signer := s.verifyWrite(w, r, body, author)
	if signer == "" {
		return ""
	}
	if signer != author {
		writeJSON(w, 401, errBody("unauthorized", "recovered signer does not match path author"))
		return ""
	}
	return signer
}

// privateReadOK gates a private-asset read: the request must carry a valid
// author signature. On failure it writes 404 (hide the asset's existence).
func (s *Server) privateReadOK(w http.ResponseWriter, r *http.Request, author string) bool {
	// Reuse verifyWrite but swallow its error body so we can 404-hide instead.
	rec := &recorder{}
	signer := s.verifyWrite(rec, r, nil, author)
	if signer == author {
		return true
	}
	writeJSON(w, 404, errBody("not_found", "asset not found"))
	return false
}

// writeMutation renders the post-write response: the fresh asset on change, or
// the idempotent {status:skip}. Used by visibility/price.
func (s *Server) writeMutation(w http.ResponseWriter, r *http.Request, typ, author, name string, changed bool, err error) {
	if err == ErrNotFound {
		writeJSON(w, 404, errBody("not_found", "asset not found"))
		return
	}
	if err != nil {
		writeJSON(w, 500, errBody("internal", err.Error()))
		return
	}
	if !changed {
		writeJSON(w, 200, map[string]string{"status": "skip", "message": "already in that state"})
		return
	}
	a, _, err := s.store.GetAsset(r.Context(), typ, author, name, "")
	if err != nil || a == nil {
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-ISANN-Nonce, X-ISANN-Timestamp")
		w.Header().Set("Access-Control-Expose-Headers", "X-ISANN-Sha256")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- tiny utils -------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func errBody(code, msg string) map[string]string {
	if msg == "" {
		msg = code
	}
	return map[string]string{"error": code, "message": msg}
}

func queryInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func clampLimit(n int) int {
	if n <= 0 || n > 100 {
		return 20
	}
	return n
}

// recorder is a throwaway ResponseWriter used to run verifyWrite without
// emitting its error body (private-read hiding writes its own 404 instead).
type recorder struct{ hdr http.Header }

func (r *recorder) Header() http.Header {
	if r.hdr == nil {
		r.hdr = http.Header{}
	}
	return r.hdr
}
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(int)             {}
