// Package version holds build information set at link time by GoReleaser
// (or by "make build") through -ldflags -X.
package version

import (
	"fmt"
	"runtime"
)

// Set with -ldflags "-X github.com/gordonmurray/outofstate.dev/internal/version.Version=...".
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a one-line description of the build.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s, %s/%s)",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
