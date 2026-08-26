// Package version carries build metadata stamped in at link time.
//
//	go build -ldflags "-X github.com/agentgate/agentgate/internal/version.Version=1.2.3 ..."
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version is the semantic version of the build. "dev" when unstamped.
	Version = "dev"
	// Commit is the git SHA of the build.
	Commit = "unknown"
	// BuildDate is the RFC3339 build timestamp.
	BuildDate = "unknown"
)

// Info is the structured build identity, surfaced on /healthz and as OTel
// resource attributes so a trace can always be tied back to an exact binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get returns the build identity of this binary.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildDate: BuildDate,
		GoVersion: runtime.Version(),
		Platform:  fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
}

// String renders a one-line version banner for logs and --version.
func String() string {
	return fmt.Sprintf("agentgate %s (commit %s, built %s, %s %s/%s)",
		Version, Commit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
