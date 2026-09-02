package main

import (
	_ "embed"
	"fmt"
	"strings"

	ver "github.com/lczyk/version/go"
)

//go:embed VERSION
var versionFile string

// fallbackVersion is the VERSION file's version, parsed the same way as the
// file itself: first non-blank, non-# line, which must be a SemVer triple.
// It's what versionLine reports when the binary carries no VCS-stamped
// version -- a plain `go build`, or a build with no git checkout at all.
func fallbackVersion() string {
	for _, line := range strings.Split(versionFile, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

// versionLine is what `--version` prints. Go marks a build from a tree with
// uncommitted changes `+dirty` in the version itself, which says it already.
func versionLine() string {
	info := ver.Read(fallbackVersion())
	if strings.HasSuffix(info.Version, "+dirty") {
		info.BuildInfo = ""
	}
	return fmt.Sprintf("asciiscript %s", info)
}
