package server

import (
	"embed"
	"io/fs"
)

// The built frontend, compiled into the binary.
//
// web/dist is a build product and is committed, for the reason every
// single-binary deployment commits one: the server is shipped by copying one
// file to a machine that has no Node on it, and a build product that is not in
// the repository is a build product the release does not have. `make ui`
// regenerates it.
//
// The all: prefix matters — without it embed skips files whose names begin
// with a dot or an underscore, and Vite emits neither today but would not warn
// if it started.
//
//go:embed all:web_dist
var webDistFS embed.FS

// webDist is the bundle rooted at its own directory, so a request for
// /assets/x.js is looked up as assets/x.js rather than web_dist/assets/x.js.
//
// A build with no bundle in it still starts and still serves the API; the
// asset handler says so in one sentence. That keeps `go build ./...` honest
// for somebody who has never run the frontend build.
func webDist() fs.FS {
	sub, err := fs.Sub(webDistFS, "web_dist")
	if err != nil {
		return webDistFS
	}
	return sub
}
