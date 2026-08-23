package api

import (
	"bytes"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/mqttview/mqttview/internal/auth"
)

// baseTag is what index.html ships with, and what gets rewritten when the UI
// is served under a prefix. Everything the page loads — assets, API calls, the
// WebSocket — is resolved against it, so this one line is the whole of the
// base-path support.
const baseTag = `<base href="/" />`

// spaHandler serves the built frontend. Anything that is not a real file falls
// back to index.html so client-side routes such as /connections/abc survive a
// page reload.
func (s *Server) spaHandler() http.Handler {
	fileServer := http.FileServer(http.FS(s.web))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if upath == "" {
			upath = "index.html"
		}

		if upath != "index.html" {
			f, err := s.web.Open(upath)
			if err == nil {
				defer f.Close()
				if info, statErr := f.Stat(); statErr == nil && !info.IsDir() {
					// Hashed asset filenames are safe to cache forever;
					// index.html must not be, or a deploy would not reach open
					// tabs.
					if strings.HasPrefix(upath, "assets/") {
						w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					} else {
						w.Header().Set("Cache-Control", "no-cache")
					}
					fileServer.ServeHTTP(w, r)
					return
				}
			}
		}
		s.serveIndex(w, r)
	})
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := s.web.Open("index.html")
	if err != nil {
		http.Error(w, "frontend is not built; run `npm run build` in web/", http.StatusNotFound)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")

	prefix := s.uiPrefix(r)
	if prefix == "" {
		if _, err := io.Copy(w, f); err != nil {
			s.log.Debug("serving index.html failed", "error", err)
		}
		return
	}

	raw, err := io.ReadAll(f)
	if err != nil {
		s.log.Error("reading index.html failed", "error", err)
		http.Error(w, "could not read the frontend", http.StatusInternalServerError)
		return
	}
	if _, err := w.Write(withBase(raw, prefix)); err != nil {
		s.log.Debug("serving index.html failed", "error", err)
	}
}

// uiPrefix is the path the browser sees the UI at, which is not the path
// mqttview receives: the Supervisor strips its own prefix before proxying.
// Only ingress mode consults the header, because outside it nothing has
// checked who set it.
func (s *Server) uiPrefix(r *http.Request) string {
	if !s.auth.IngressMode() {
		return ""
	}
	return auth.IngressPath(r)
}

// withBase rewrites the base href so relative URLs resolve under the prefix.
//
// A string replacement rather than an HTML parser: the input is our own build
// output with one known line in it, and a parser would be a dependency and a
// second way for the page to differ from what was built.
//
// The prefix reaches here having already matched auth.ingressPathPattern,
// which admits only unreserved URL characters — no quote, angle bracket or
// ampersand can be in it. It is escaped again anyway, so that the safety does
// not depend on a caller remembering to validate first.
func withBase(index []byte, prefix string) []byte {
	replacement := `<base href="` + template.HTMLEscapeString(prefix) + `/" />`
	if bytes.Contains(index, []byte(baseTag)) {
		return bytes.Replace(index, []byte(baseTag), []byte(replacement), 1)
	}
	// No base tag to rewrite: the frontend was built from a different tree.
	// Inserting one after <head> is better than serving a page whose every
	// asset URL is wrong.
	if i := bytes.Index(index, []byte("<head>")); i >= 0 {
		at := i + len("<head>")
		out := make([]byte, 0, len(index)+len(replacement))
		out = append(out, index[:at]...)
		out = append(out, []byte(replacement)...)
		return append(out, index[at:]...)
	}
	return index
}

// SubFS narrows an embedded filesystem to the directory holding the built
// assets, so URLs do not include the build directory name.
func SubFS(fsys fs.FS, dir string) (fs.FS, error) {
	return fs.Sub(fsys, dir)
}
