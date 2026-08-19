package api

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

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

		f, err := s.web.Open(upath)
		if err == nil {
			defer f.Close()
			if info, statErr := f.Stat(); statErr == nil && !info.IsDir() {
				// Hashed asset filenames are safe to cache forever; index.html
				// must not be, or a deploy would not reach open tabs.
				if strings.HasPrefix(upath, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		s.serveIndex(w)
	})
}

func (s *Server) serveIndex(w http.ResponseWriter) {
	f, err := s.web.Open("index.html")
	if err != nil {
		http.Error(w, "frontend is not built; run `npm run build` in web/", http.StatusNotFound)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if _, err := io.Copy(w, f); err != nil {
		s.log.Debug("serving index.html failed", "error", err)
	}
}

// SubFS narrows an embedded filesystem to the directory holding the built
// assets, so URLs do not include the build directory name.
func SubFS(fsys fs.FS, dir string) (fs.FS, error) {
	return fs.Sub(fsys, dir)
}
