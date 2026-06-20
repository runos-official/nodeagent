// Package version holds the RunOS node agent's build version.
package version

// Version is the node agent's version. It defaults to "dev" for local builds
// and is injected at build time from the git tag via:
//
//	-ldflags "-X github.com/runos-official/nodeagent/version.Version=<v>"
//
// The release pipeline (.github/workflows/release.yml) sets it to the tag
// minus the leading "v"; the Makefile sets it from the latest git tag. It is a
// var (not a const) precisely so -ldflags can override it at link time.
var Version = "dev"
