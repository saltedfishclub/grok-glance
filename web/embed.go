// Package web carries the built frontend into the binary.
//
// The embed directive points at `dist/`, which is Vite's output. That directory
// is checked in with only a `.gitkeep` so a clean clone still compiles: Go
// resolves `//go:embed` at build time and would fail outright on a missing path,
// which would mean `go build ./...` could not run until someone had installed
// npm. Assets returns an error in that state instead, and the server falls back
// to a placeholder page telling the operator to run `make web`.
package web

import (
	"embed"
	"errors"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// ErrNotBuilt means the binary was built without running the frontend build.
var ErrNotBuilt = errors.New("web UI not built; run `make web`")

// Assets returns the frontend rooted at index.html.
func Assets() (fs.FS, error) {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(dist, "index.html"); err != nil {
		return nil, ErrNotBuilt
	}
	return dist, nil
}
