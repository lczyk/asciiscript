package main

import (
	"os"
	"strings"
	"testing"

	"github.com/lczyk/assert"
)

func TestWantsVersion(t *testing.T) {
	for _, args := range [][]string{
		{"--version"},
		{"-v"},
		{"demo.sh", "demo.cast", "--version"},
	} {
		assert.That(t, wantsVersion(args), "wantsVersion(%q)", args)
	}
	for _, args := range [][]string{
		{},
		{"demo.sh", "demo.cast"},
		{"-q", "--speed", "2"},
	} {
		assert.That(t, !wantsVersion(args), "wantsVersion(%q)", args)
	}
}

func TestVersionLineCarriesTheVersion(t *testing.T) {
	assert.That(t, strings.Contains(versionLine(), Version), "version line should name the version")
}

// The generator only accepts a SemVer VERSION file, but a build off a fresh
// clone never runs it, so the check would go unnoticed until release time.
func TestVersionFileIsThere(t *testing.T) {
	b, err := os.ReadFile("VERSION")
	assert.NoError(t, err)
	var ver string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			ver = line
			break
		}
	}
	assert.That(t, strings.Count(ver, ".") == 2, "VERSION %q should be a semver triple", ver)
}
