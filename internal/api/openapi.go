package api

import (
	"net/http"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/openapi"
)

// HandleOpenAPISpec serves the bundled OpenAPI specification.
// GET-only; returns 405 for other methods.
func (s *Server) HandleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(openapi.SpecJSON)
}

// HandleDocs serves the embedded RapiDoc HTML interface and its assets.
// GET /docs         → docs/index.html (text/html)
// GET /docs/xxx     → asset from embedded docs/ dir
// Anything else     → 404
func (s *Server) HandleDocs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/docs")
	switch {
	case path == "" || path == "/":
		// Serve index.html
		data, err := openapi.DocsFS.ReadFile("docs/index.html")
		if err != nil {
			s.writeJSONError(w, http.StatusInternalServerError, "failed to read docs")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	case strings.HasPrefix(path, "/"):
		// Serve asset (e.g. /rapidoc-min.js)
		assetPath := "docs" + path
		data, err := openapi.DocsFS.ReadFile(assetPath)
		if err != nil {
			s.writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		// Set content type based on extension.
		if strings.HasSuffix(path, ".js") {
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		} else if strings.HasSuffix(path, ".html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		} else if strings.HasSuffix(path, ".css") {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		w.Write(data)
	default:
		s.writeJSONError(w, http.StatusNotFound, "not found")
	}
}
