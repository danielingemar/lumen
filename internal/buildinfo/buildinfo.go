// Package buildinfo carries the version of this build. The Dockerfile sets it from the source code (a short hash), so
// the same source gives the same version on every server and in every agent; it is shown in the web UI and used to
// flag agents that are older than the server.
package buildinfo

// Version is "dev" for a plain `go build`, "src-1a2b3c4d" for a Docker build, or whatever --build-arg VERSION says.
var Version = "dev"
