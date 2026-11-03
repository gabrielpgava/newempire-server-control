package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distribution embed.FS

// FS contains the built administration UI. Run npm ci && npm run build in web first.
func FS() fs.FS {
	files, err := fs.Sub(distribution, "dist")
	if err != nil {
		panic(err)
	}
	return files
}
