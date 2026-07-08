package main

import (
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

// replay concatenates a plan's keystrokes. With no fabricated mistakes, human
// typing only ever inserts the literal characters, so the result must equal the
// original line.
func replay(ks []keystroke) string {
	var b strings.Builder
	for _, k := range ks {
		b.WriteString(k.data)
	}
	return b.String()
}

func TestHumanPlanReproducesLine(t *testing.T) {
	lines := []string{
		"echo \"hi, this is asciiscript\"\n",
		"for i in 1 2 3; do echo \"  line $i\"; done\n",
		"the quick brown fox jumps over the lazy dog\n",
		"ls\n",
	}
	for seed := int64(0); seed < 200; seed++ {
		h := newHuman(seed)
		for _, line := range lines {
			assert.Equal(t, replay(h.plan(line, 40*time.Millisecond)), line)
		}
	}
}

func TestHumanDeterministicForSeed(t *testing.T) {
	line := "echo hello world\n"
	a := newHuman(1234).plan(line, 40*time.Millisecond)
	b := newHuman(1234).plan(line, 40*time.Millisecond)
	assert.Equal(t, len(a), len(b))
	for i := range a {
		assert.Equal(t, a[i].data, b[i].data)
		assert.Equal(t, a[i].pause, b[i].pause)
	}
}

func TestHumanPausesWithinBounds(t *testing.T) {
	base := 40 * time.Millisecond
	ceil := time.Duration(float64(base) * pauseCeilFactor)
	h := newHuman(99)
	for _, k := range h.plan("some command with words\n", base) {
		assert.That(t, k.pause >= 0, "pause is non-negative")
		assert.That(t, k.pause <= ceil, "pause is capped")
	}
}

func TestUniform(t *testing.T) {
	ks := uniform("abc\n", 40*time.Millisecond)
	assert.Len(t, ks, 4)
	assert.Equal(t, replay(ks), "abc\n")
	for _, k := range ks {
		assert.Equal(t, k.pause, 40*time.Millisecond)
	}
}

// Alternating-hand digraphs type faster than same-finger reaches.
func TestTransMultDigraphs(t *testing.T) {
	assert.That(t, transMult('t', 'h') < transMult('e', 'c'),
		"alt-hand (th) should be quicker than same-finger (ec)")
	assert.Equal(t, transMult(' ', 'a'), spaceFactor)
	assert.Equal(t, transMult('.', 'a'), punctFactor)
}
