package tunnel

import (
	"net/http"

	"golang.org/x/net/webdav"
)

// NewWebDAVHandler creates a WebDAV handler serving the given directory.
func NewWebDAVHandler(dir string, prefix string, readOnly bool) http.Handler {
	h := &webdav.Handler{
		Prefix:     prefix,
		FileSystem: webdav.Dir(dir),
		LockSystem: webdav.NewMemLS(),
	}

	if !readOnly {
		return h
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PUT", "DELETE", "MKCOL", "COPY", "MOVE", "PROPPATCH":
			http.Error(w, "read-only", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}
