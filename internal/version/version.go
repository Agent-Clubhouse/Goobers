// Package version exposes build metadata for all Goobers control-plane binaries.
//
// The values are intended to be overridden at build time via -ldflags, e.g.:
//
//	go build -ldflags "-X github.com/goobers/goobers/internal/version.Version=v1.2.3 \
//	  -X github.com/goobers/goobers/internal/version.Commit=$(git rev-parse --short HEAD)"
//
// See the Makefile's LDFLAGS for the canonical wiring.
package version

import (
	"fmt"
	"runtime"
)

// Build metadata. Overridden via -ldflags at build time; the defaults below are
// what a plain `go build` (or `go run`) produces.
var (
	// Version is the semantic version of the build (e.g. "v1.2.3"), or "dev".
	Version = "dev"
	// Commit is the short git SHA the build was produced from, or "none".
	Commit = "none"
	// Date is the build timestamp (RFC 3339), or "unknown".
	Date = "unknown"
)

// Info is a structured snapshot of the build metadata plus the Go runtime.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// Get returns the current build Info.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// String renders a single-line human-readable summary, e.g.
// "dev (commit none, built unknown, go1.23 darwin/arm64)".
func (i Info) String() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s %s)",
		i.Version, i.Commit, i.Date, i.GoVersion, i.Platform)
}
