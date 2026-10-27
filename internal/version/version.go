// Package version exposes build metadata for drydock.
package version

// These values are overridden at build time via -ldflags.
var (
	// Version is the semantic version of the build.
	Version = "0.1.0-dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// Date is the build date in RFC3339 format.
	Date = "unknown"
)

// UserAgent is sent to registries and the Docker daemon.
func UserAgent() string { return "drydock/" + Version }
