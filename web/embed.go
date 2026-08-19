// Package webui embeds the built single-page frontend so that mqttview ships
// as one binary with no runtime asset directory.
//
// `dist` is produced by `npm run build` in this directory. A placeholder is
// committed so a fresh checkout compiles before the frontend is built.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the built frontend rooted at dist/.
func FS() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
