package main

import (
	"fmt"

	ver "github.com/lczyk/version/go"
)

//go:generate go run github.com/lczyk/version/go/cmd/generate-version -out version_gen.go -pkg main -init

// Build stamps, filled in by version_gen.go's init from the VERSION file and
// the git state at build time. A tree built without `make generate-version`
// keeps these defaults.
var (
	Version   = "0.0.0-dev"
	CommitSHA string
	BuildDate string
	BuildInfo string
)

// wantsVersion reports whether the args ask for the version. It's checked
// before go-flags parses, so `--version` isn't refused for want of the
// required positional arguments.
func wantsVersion(args []string) bool {
	for _, a := range args {
		if a == "--version" || a == "-v" {
			return true
		}
	}
	return false
}

// versionLine is what `--version` prints.
func versionLine() string {
	return fmt.Sprintf("asciiscript %s", ver.FormatVersion(Version, CommitSHA, BuildDate, BuildInfo))
}
