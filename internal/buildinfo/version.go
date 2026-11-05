// Package buildinfo carries the version stamped into a binary at build time:
//
//	go build -ldflags "-X github.com/priytamjeepandey/forge-ci/internal/buildinfo.Version=1.9.0" ./cmd/server
//
// Both the server and the runner are stamped from the same source tree, so
// a runner whose version differs from the server's is out of date.
package buildinfo

import "runtime"

// Version is the release version ("dev" when not stamped).
var Version = "dev"

// Platform is the GOOS-GOARCH pair, matching the runner download names.
var Platform = runtime.GOOS + "-" + runtime.GOARCH
