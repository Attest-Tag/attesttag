// Package ui embeds the built admin console (Next.js static export in ./out).
// Build it with `npm run build` inside ui/; the Go binary serves it at /admin/.
//
// out/ is gitignored, and out/index.html is force-added to git anyway. That looks like a
// mistake and is not: //go:embed fails the compile when its pattern matches nothing, so
// without one committed file a fresh clone cannot `go build` — or `go test ./...` — until
// somebody has run `make ui`. The error it gives is "pattern all:out: no matching files
// found", which says nothing about npm.
//
// A .gitkeep would be the tidier placeholder and does not work: `next build` empties out/ of
// anything it did not put there, so the placeholder disappears on every build and shows up as
// a deletion. index.html survives because Next writes one — it is the only file that can hold
// this open. The cost is that a rebuilt console leaves that file modified in `git status`.
//
// The container does not use the committed copy: the Dockerfile builds the console in its own
// stage and copies out/ over the top, and .dockerignore excludes out/ from the build context
// so a stale local export cannot reach an image.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:out
var files embed.FS

// FS returns the exported site rooted at out/.
func FS() fs.FS {
	sub, err := fs.Sub(files, "out")
	if err != nil {
		panic(err)
	}
	return sub
}
