// Package adminui embeds the built Admin UI (admin-ui/, a Vite + React +
// TypeScript SPA) into the runkite binary, so `runkite serve` ships one
// executable with a working dashboard -- no separate deploy step, no
// Node.js runtime dependency for end users (master plan: "one
// executable"). The React source lives in admin-ui/; its Vite config
// builds straight into this package's dist/ subdirectory (see
// admin-ui/vite.config.ts), which is committed to the repo so `go build`
// works without ever needing npm -- only UI contributors touching
// admin-ui/ need Node.js, and they re-run the build to refresh dist/.
package adminui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"time"
)

//go:embed all:dist
var distFS embed.FS

// Handler returns an http.Handler serving the built SPA at whatever
// prefix the caller mounts it under (see cmd/serve.go, mounted at
// /admin/). Falls back to index.html for any path that isn't a real
// file, so client-side routes (e.g. /admin/threads/abc) resolve to the
// SPA shell instead of 404ing on a hard refresh.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Only possible if the embed directive itself is broken (a build
		// error, not a runtime one) -- fail loudly rather than serve
		// nothing silently.
		panic("adminui: dist not embedded: " + err.Error())
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		panic("adminui: dist/index.html missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := trimLeadingSlash(r.URL.Path)
		if p == "" || p == "index.html" {
			// Serve index.html's bytes directly rather than delegating to
			// fileServer for this specific name: net/http's FileServer
			// always 301-redirects a literal "index.html" request to
			// "./", which would otherwise loop right back here for the
			// SPA-fallback case just below.
			http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
			return
		}
		if _, statErr := fs.Stat(sub, p); statErr != nil {
			http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func trimLeadingSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p[1:]
	}
	return p
}
