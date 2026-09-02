package main

import (
	"strings"
	"testing"

	"github.com/lczyk/assert"
)

func TestVersionLineCarriesTheVersion(t *testing.T) {
	// go test builds report no VCS-stamped module version, so Read falls back
	// to fallbackVersion.
	assert.That(t, strings.Contains(versionLine(), fallbackVersion()), "version line should name the version")
}

// Nothing else checks that VERSION holds a semver triple, so a malformed
// file would otherwise go unnoticed until release time.
func TestFallbackVersionIsSemverTriple(t *testing.T) {
	v := fallbackVersion()
	assert.That(t, strings.Count(v, ".") == 2, "VERSION %q should be a semver triple", v)
}
