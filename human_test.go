package main

import (
	"testing"
	"time"

	"github.com/lczyk/assert"
)

// replay applies a keystroke plan the way a terminal line editor would: text
// inserts at the cursor, backspace erases before it, arrows move it. The result
// is what actually ends up on the command line.
func replay(ks []keystroke) string {
	var b []rune
	cur := 0
	for _, k := range ks {
		switch k.data {
		case keyBackspace:
			if cur > 0 {
				b = append(b[:cur-1], b[cur:]...)
				cur--
			}
		case keyLeft:
			if cur > 0 {
				cur--
			}
		case keyRight:
			if cur < len(b) {
				cur++
			}
		default: // literal text, inserted at the cursor
			for _, r := range k.data {
				b = append(b, 0)
				copy(b[cur+1:], b[cur:])
				b[cur] = r
				cur++
			}
		}
	}
	return string(b)
}

// The core invariant: however much a plan jitters, hesitates, or fat-fingers,
// replaying it must reproduce the original line exactly.
func TestHumanPlanReproducesLine(t *testing.T) {
	lines := []string{
		"echo \"hi, this is asciiscript\"\n",
		"for i in 1 2 3; do echo \"  line $i\"; done\n",
		"git commit -m 'wip: a fairly long-ish message with punctuation.'\n",
		"the quick brown fox jumps over the lazy dog again and again\n",
		"ls\n",
	}
	sawArrow := false
	// many seeds so typo / omission / hesitation / arrow-correction branches fire
	for seed := int64(0); seed < 500; seed++ {
		h := newHuman(seed)
		for _, line := range lines {
			ks := h.plan(line, 40*time.Millisecond)
			assert.Equal(t, replay(ks), line)
			for _, k := range ks {
				if k.data == keyLeft {
					sawArrow = true
				}
			}
		}
	}
	// the arrow-back-and-insert correction path must be reachable, else the
	// invariant above is only proving the easy cases.
	assert.That(t, sawArrow, "arrow-correction path should be exercised across seeds")
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

func TestNeighbourOnRow(t *testing.T) {
	h := newHuman(7)
	// 's' neighbours are 'a' and 'd'
	n, ok := h.neighbour('s')
	assert.That(t, ok, "'s' should map")
	assert.That(t, n == 'a' || n == 'd', "neighbour of s is a or d")

	// uppercase preserved
	n, ok = h.neighbour('S')
	assert.That(t, ok, "'S' should map")
	assert.That(t, n == 'A' || n == 'D', "neighbour of S is A or D")

	// non-letters aren't mapped
	_, ok = h.neighbour('5')
	assert.That(t, !ok, "digit has no neighbour")
}
