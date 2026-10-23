// Package version reports the release identity of this build.
package version

// These are overridden at build time with -ldflags -X. The defaults keep a
// plain "go build" usable during development.
var (
	// Value is the release tag, for example v1.0.1.
	Value = "dev"
	// Commit is the short commit the binary was built from.
	Commit = "unknown"
	// Date is the build time in UTC.
	Date = "unknown"
)
