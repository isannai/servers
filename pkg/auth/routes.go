package auth

import (
	"net/http"
	"strings"
)

// Route permission level.
const (
	LevelNone  = "none"  // public, no signature required
	LevelUser  = "user"  // signature required in protected mode (skipped in public mode)
	LevelAdmin = "admin" // signature required, must be owner or admin
)

// ClassifyRoute classifies a Broker HTTP route by required permission level.
//
// Returns one of:
//   - LevelNone:  no signature required (health, info, public listings, auth endpoint, SPA, gate/rendezvous proxies, GET on shared catalog)
//   - LevelUser:  service consumption (node svc)
//   - LevelAdmin: management endpoints (my-nodes auth, provider/installer/terminal, admin API, local installer)
//
// Reference: docs/2026-04-06/broker-auth.md §1-1
func ClassifyRoute(method, path string) string {
	// Normalise method
	method = strings.ToUpper(method)

	// === Public (no signature) ===
	switch path {
	case "/", "/health", "/info", "/node-id", "/v1/nodes", "/v1/auth/verify":
		return LevelNone
	}
	// SPA static assets — anything that's not /v1/, /node/, /gate/, /rendezvous/ falls through to the SPA handler
	if !strings.HasPrefix(path, "/v1/") &&
		!strings.HasPrefix(path, "/node/") &&
		!strings.HasPrefix(path, "/gate/") &&
		!strings.HasPrefix(path, "/rendezvous/") &&
		!strings.HasPrefix(path, "/auth") {
		return LevelNone
	}
	// Gate proxy / Rendezvous proxy → no signature (read-only proxies)
	if strings.HasPrefix(path, "/gate/") {
		return LevelNone
	}
	if strings.HasPrefix(path, "/rendezvous/") {
		return LevelNone
	}

	// === Admin: my-nodes auth proxy (CRUD moved to browser IndexedDB) ===
	if strings.HasPrefix(path, "/v1/my-nodes/") {
		return LevelAdmin
	}
	// (Note: /v1/local/* removed entirely. Provider bootstrap is via CLI;
	// post-bootstrap installs go through /node/{id}/installer/*.)
	// Admin: monitoring/admin API
	if strings.HasPrefix(path, "/v1/admin/") {
		return LevelAdmin
	}

	// === User: node svc ===
	if strings.HasPrefix(path, "/node/") {
		// /node/{id}/svc/* → user level (service consumption)
		// /node/{id}/provider/*, /installer/* → admin level
		rest := strings.TrimPrefix(path, "/node/")
		// Strip nodeID
		if idx := strings.Index(rest, "/"); idx >= 0 {
			sub := rest[idx+1:]
			if strings.HasPrefix(sub, "svc/") {
				return LevelUser
			}
			// Public profile reads — emblem/about/file are shown on the public node list
			if method == http.MethodGet && (sub == "provider/emblem" || sub == "provider/about" || sub == "provider/file" || sub == "provider/gallery") {
				return LevelNone
			}
			if strings.HasPrefix(sub, "provider/") || strings.HasPrefix(sub, "installer/") {
				return LevelAdmin
			}
		}
		// Default for /node/* → admin (safer)
		return LevelAdmin
	}

	// Default: admin (safer for unknown /v1/* paths)
	return LevelAdmin
}
