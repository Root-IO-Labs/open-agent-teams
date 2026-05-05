// Package version exposes the build-time version metadata for oat.
//
// The values below default to development-friendly sentinels ("dev" / "none")
// so a plain `go run ./...` keeps working; release builds override them via
// linker flags:
//
//	go build -ldflags "-X github.com/Root-IO-Labs/open-agent-teams/internal/version.Version=v0.1.0 \
//	                   -X github.com/Root-IO-Labs/open-agent-teams/internal/version.Commit=$(git rev-parse --short HEAD) \
//	                   -X github.com/Root-IO-Labs/open-agent-teams/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// The canonical wiring lives in the top-level Makefile (VERSION / COMMIT /
// DATE targets) and in .goreleaser.yml, so hand-rolled invocations should
// match that shape to keep `oat version` stable across build methods.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is the human-readable semver string for the running binary.
// Overridden by ldflags at release time; otherwise stays as "dev".
var Version = "dev"

// Commit is the short git commit SHA the binary was built from. "none"
// when unavailable (e.g. `go install` outside a git checkout).
var Commit = "none"

// Date is the UTC build timestamp in RFC-3339 format. "unknown" when the
// build system didn't inject it.
var Date = "unknown"

// Info captures the three release-identity fields together so callers can
// render them with a single struct/JSON encode instead of three globals.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	IsDev   bool   `json:"isDev"`
}

// Current returns the build metadata for the currently-running binary.
// It falls back to Go's VCS stamps (debug.ReadBuildInfo) when Commit is
// still the zero value; this lets `go install ...@sha` picks up a useful
// short-SHA automatically even without ldflags.
func Current() Info {
	v := Version
	c := Commit
	d := Date
	if c == "none" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					if len(s.Value) >= 7 {
						c = s.Value[:7]
					} else if s.Value != "" {
						c = s.Value
					}
				case "vcs.time":
					if d == "unknown" && s.Value != "" {
						d = s.Value
					}
				}
			}
		}
	}
	return Info{
		Version: v,
		Commit:  c,
		Date:    d,
		IsDev:   strings.HasPrefix(v, "dev") || v == "",
	}
}

// String returns a compact "oat vX.Y.Z (sha, date)" rendering suitable for
// the `oat version` default output.
func (i Info) String() string {
	var b strings.Builder
	b.WriteString("oat ")
	if i.Version == "" {
		b.WriteString("dev")
	} else {
		b.WriteString(i.Version)
	}
	extras := make([]string, 0, 2)
	if i.Commit != "" && i.Commit != "none" {
		extras = append(extras, i.Commit)
	}
	if i.Date != "" && i.Date != "unknown" {
		extras = append(extras, i.Date)
	}
	if len(extras) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(extras, ", "))
		b.WriteString(")")
	}
	return b.String()
}
