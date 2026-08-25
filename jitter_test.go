package main

import (
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

// replay concatenates a plan's keystrokes. jitter only ever types the literal
// characters (it shapes timing, not text), so the result must equal the line.
func replay(ks []keystroke) string {
	var b strings.Builder
	for _, k := range ks {
		b.WriteString(k.data)
	}
	return b.String()
}

func TestJitterReproducesLine(t *testing.T) {
	lines := []string{
		"echo \"hi, this is asciiscript\"\n",
		"for i in 1 2 3; do echo \"  line $i\"; done\n",
		"the quick brown fox jumps over the lazy dog\n",
		"ls\n",
	}
	for _, scale := range []float64{0, 0.5, 1, 2} {
		for seed := int64(0); seed < 100; seed++ {
			j := newJitter(scale, seed)
			for _, line := range lines {
				assert.Equal(t, replay(j.plan(line, 40*time.Millisecond)), line)
			}
		}
	}
}

// scale 0 is exactly uniform: every pause is the base delay.
func TestJitterScaleZeroIsUniform(t *testing.T) {
	base := 40 * time.Millisecond
	j := newJitter(0, 1)
	for _, k := range j.plan("echo \"a, b; c\" | grep x\n", base) {
		assert.Equal(t, k.pause, base)
	}
}

func TestJitterDeterministicForSeed(t *testing.T) {
	line := "echo hello world\n"
	a := newJitter(1, 1234).plan(line, 40*time.Millisecond)
	b := newJitter(1, 1234).plan(line, 40*time.Millisecond)
	assert.Equal(t, len(a), len(b))
	for i := range a {
		assert.Equal(t, a[i].data, b[i].data)
		assert.Equal(t, a[i].pause, b[i].pause)
	}
}

func TestJitterPausesWithinBounds(t *testing.T) {
	base := 40 * time.Millisecond
	ceil := time.Duration(float64(base) * pauseCeilFactor)
	j := newJitter(1, 99)
	for _, k := range j.plan("some command with words\n", base) {
		assert.That(t, k.pause >= 0, "pause is non-negative")
		assert.That(t, k.pause <= ceil, "pause is capped")
	}
}

// Alternating-hand digraphs type faster than same-finger reaches.
func TestTransMultDigraphs(t *testing.T) {
	assert.That(t, transMult('t', 'h') < transMult('e', 'c'),
		"alt-hand (th) should be quicker than same-finger (ec)")
	assert.Equal(t, transMult(' ', 'a'), spaceFactor)
	assert.Equal(t, transMult('.', 'a'), punctFactor)
}
