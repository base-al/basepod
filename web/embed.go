// Package web embeds the built dashboard (web/dist, produced by `make ui`
// / `npm run build`) into the basepod binary and serves it as a
// single-page app.
//
// Only web/dist/index.html is committed to the repo, as a placeholder
// ("BasePod dashboard not built — run make ui") so `go build ./...` works
// without Node installed. `make ui` overwrites the whole dist/ directory
// with the real build locally and in CI; the built output must never be
// committed.
package web

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// indexPage is the SPA shell every client-side route falls back to.
const indexPage = "index.html"

//go:embed all:dist
var distFS embed.FS

// Handler serves the embedded dashboard build with SPA fallback: real
// files under dist/ are served as-is (hashed /assets/* files get a
// long-lived immutable cache header), and any other GET for an
// extensionless path or a request that accepts HTML falls back to
// index.html (which is always served with Cache-Control: no-cache, since
// it references the currently-hashed asset filenames). A path that looks
// like a static asset request (has a file extension) but doesn't exist
// 404s instead of masking the miss with the SPA shell.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Can only happen if the "dist" directory embed above stops
		// matching this function's fs.Sub call — a build-time
		// programmer error, not a runtime condition.
		panic("web: dist not embedded: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleaned := path.Clean("/" + r.URL.Path)
		name := strings.TrimPrefix(cleaned, "/")
		if name == "" {
			name = indexPage
		}

		if f, err := sub.Open(name); err == nil {
			info, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !info.IsDir() {
				if name == indexPage {
					// http.FileServer redirects bare "index.html"
					// requests to "./" rather than serving the file
					// directly, so index.html always goes through our
					// own ServeContent path instead.
					serveIndex(w, r, sub)
					return
				}
				setCacheHeaders(w, cleaned)
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/" + name
				fileServer.ServeHTTP(w, r2)
				return
			}
		}

		// No exact file match. SPA fallback for client-side routes:
		// extensionless paths, or requests that explicitly accept HTML.
		// Anything with a file extension (a missing asset) 404s instead.
		if path.Ext(cleaned) == "" || strings.Contains(r.Header.Get("Accept"), "text/html") {
			serveIndex(w, r, sub)
			return
		}

		http.NotFound(w, r)
	})
}

// setCacheHeaders applies BasePod's caching policy for a non-index file
// that is about to be served as-is: hashed build assets under /assets/
// are immutable and cached for a year. index.html is handled separately
// by serveIndex, which always sets Cache-Control: no-cache.
func setCacheHeaders(w http.ResponseWriter, cleaned string) {
	if strings.HasPrefix(cleaned, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
}

// serveIndex writes index.html as the SPA fallback response, always with
// Cache-Control: no-cache.
func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	f, err := sub.Open(indexPage)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, indexPage, info.ModTime(), rs)
}
