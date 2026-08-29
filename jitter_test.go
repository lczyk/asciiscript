package main

import (
	"strconv"
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
				assert.Equal(t, replay(j.plan(line, 40*time.Millisecond, 0)), line)
			}
		}
	}
}

// scale 0 is exactly uniform: every pause is the base delay, the line gap in
// front of the first key included.
func TestJitterScaleZeroIsUniform(t *testing.T) {
	base := 40 * time.Millisecond
	j := newJitter(0, 1)
	for _, k := range j.plan("echo \"a, b; c\" | grep x\n", base, 0) {
		assert.Equal(t, k.pause, base)
	}
}

// At full scale the line gap is a boundary like a space or a full stop: longer
// than an ordinary key, and jittered.
func TestJitterLineGapIsABoundary(t *testing.T) {
	base := 40 * time.Millisecond
	var sum time.Duration
	const n = 200
	for seed := int64(0); seed < n; seed++ {
		sum += newJitter(1, seed).linePause(base, 0)
	}
	mean := sum / n
	assert.That(t, mean > base*2 && mean < base*4, "mean line gap "+mean.String()+" should be a few base delays")
}

// A pause the script asked for is what the line starts with: exactly that at
// scale 0, and jittered around it -- never clamped against the much smaller
// per-key delay -- otherwise.
func TestJitterLinePauseHonoursTheScript(t *testing.T) {
	base, pause := 40*time.Millisecond, 2*time.Second
	assert.Equal(t, newJitter(0, 1).plan("ab\n", base, pause)[0].pause, pause)

	var lo, hi time.Duration = pause, 0
	var above int
	const n = 200
	for seed := int64(0); seed < n; seed++ {
		p := newJitter(1, seed).linePause(base, pause)
		lo, hi = min(lo, p), max(hi, p)
		if p > pause {
			above++
		}
	}
	assert.That(t, lo < pause && hi > pause, "should vary either side of the pause asked for")
	assert.That(t, above > n/3 && above < 2*n/3, "should be centred on it")
	floor := time.Duration(float64(pause) * pauseFloorFactor)
	ceil := time.Duration(float64(pause) * pauseCeilFactor)
	assert.That(t, lo >= floor && hi <= ceil, "should be clamped against the pause, not the delay")
}

func TestJitterDeterministicForSeed(t *testing.T) {
	line := "echo hello world\n"
	a := newJitter(1, 1234).plan(line, 40*time.Millisecond, 0)
	b := newJitter(1, 1234).plan(line, 40*time.Millisecond, 0)
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
	for _, k := range j.plan("some command with words\n", base, 0) {
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

// --jitter asks for more variation, so cranking it must not quietly turn into
// less. The sub-1 digraph factors fade towards a negative multiplier past a
// scale of ~10; unfloored, that collapses alternating-hand pairs -- half the
// keystrokes in ordinary prose -- to no pause at all.
func TestJitterStaysHumanAtHighScales(t *testing.T) {
	base := 40 * time.Millisecond
	floor := time.Duration(float64(base) * pauseFloorFactor)
	ceil := time.Duration(float64(base) * pauseCeilFactor)

	for _, scale := range []float64{2, 5, 11, 20, 100} {
		var under, over int
		for seed := int64(0); seed < 200; seed++ {
			for _, k := range newJitter(scale, seed).plan("the quick brown fox\n", base, 0) {
				if k.pause < floor {
					under++
				}
				if k.pause > ceil {
					over++
				}
			}
		}
		at := " at scale " + strconv.FormatFloat(scale, 'g', -1, 64)
		assert.Equal(t, under, 0, "every pause should be a real gap"+at)
		assert.Equal(t, over, 0, "no pause should read as a hang"+at)
	}
}

// The model shapes timing, never text -- including for a script that isn't
// valid UTF-8. Planning through []rune would silently retype a stray byte as
// the replacement character.
func TestJitterTypesInvalidUTF8Verbatim(t *testing.T) {
	for _, line := range []string{"\xd7", "echo \"caf\xe9\"\n", "\xff\xfe\x00", "ok\n"} {
		ks := newJitter(1, 7).plan(line, 40*time.Millisecond, 0)
		assert.Equal(t, replay(ks), line)
		assert.Equal(t, len(ks), len([]rune(line)), line) // one keystroke per rune-or-stray-byte
	}
}
