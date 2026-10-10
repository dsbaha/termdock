// Package web serves the embedded frontend and wires a small SPA fallback.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var staticFiles embed.FS

// TerminalPage serves the standalone popup terminal page (t.html). The page
// itself carries no session data — it reads the session id from the URL path
// and attaches to /api/sessions/{id}/attach, which enforces authorization.
// Callers must gate this behind authentication.
func TerminalPage() http.Handler {
	data, err := staticFiles.ReadFile("static/t.html")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
}

// NotAuthenticatedPage is shown in a popup window when the visitor has no
// valid login session.
func NotAuthenticatedPage() http.Handler {
	const page = `<!DOCTYPE html><html><head><meta charset="utf-8"><title>TermDock</title>
<style>body{background:#0d1117;color:#e6edf3;font-family:sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}</style>
</head><body><p>Not signed in. Please log in via the TermDock dashboard, then reopen this window.</p></body></html>`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(page))
	})
}

// Handler serves the frontend from web/static. Unknown non-API paths fall
// back to index.html so the dashboard always boots.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && !strings.Contains(r.URL.Path[1:], "/") && !strings.Contains(r.URL.Path, ".") {
			r.URL.Path = "/"
		}
		// Prevent caching of the entry point during development iterations.
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}
