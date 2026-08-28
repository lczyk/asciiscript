package main

import (
	"math"
	"math/rand"
	"strings"
	"time"
	"unicode/utf8"
)

// keystroke is one unit of typing: bytes to write, then a pause to sleep after.
// A []keystroke plan is the entire API surface between the jitter subsystem
// (which produces plans) and Shell.Run (which replays them onto the pty).
type keystroke struct {
	data  string
	pause time.Duration
}

// jitter plans human-looking keystroke timing for a line. Instead of a uniform
// delay between keys, each pause is shaped by a model fitted to real captured
// typing (see the constants below). It never alters the text -- only the timing
// -- so a plan always types the line exactly.
//
// scale sets the intensity: 1 is the full, human-like effect; values below 1
// ease every deviation back toward uniform; 0 is exactly uniform (each pause is
// the base delay). Timing is seeded, so a run is reproducible.
type jitter struct {
	rng   *rand.Rand
	scale float64
}

// newJitter builds a jitter planner. scale >= 0 (0 = uniform); seed makes the
// timing reproducible.
func newJitter(scale float64, seed int64) *jitter {
	return &jitter{rng: rand.New(rand.NewSource(seed)), scale: scale}
}

// plan turns a line into keystrokes typed at the given base per-key delay. Each
// keystroke carries the line's own bytes rather than a re-encoded rune, so a
// script that isn't valid UTF-8 still types exactly as written.
func (j *jitter) plan(line string, base time.Duration) []keystroke {
	ks := make([]keystroke, 0, len(line))
	var prev rune
	for i, w := 0, 0; i < len(line); i += w {
		var ch rune
		ch, w = utf8.DecodeRuneInString(line[i:])
		ks = append(ks, keystroke{line[i : i+w], j.pauseFor(prev, ch, base)})
		prev = ch
	}
	return ks
}

// pauseFor is the delay before typing ch, given the char before it. Three
// effects stack on the base delay, each faded by scale (so scale 0 -> base):
//   - a boundary/digraph multiplier (word gaps, punctuation, finger physics)
//   - lognormal jitter
//   - the occasional hesitation
func (j *jitter) pauseFor(prev, ch rune, base time.Duration) time.Duration {
	s := j.scale
	mult := max(1+(transMult(prev, ch)-1)*s, minTransMult)
	p := float64(base) * mult * math.Exp(j.rng.NormFloat64()*jitterSigma*s)
	d := time.Duration(p)
	if j.rng.Float64() < hesitationProb {
		d += time.Duration(float64(j.factor(base, hesitationMin, hesitationMax)) * s)
	}
	return clampPause(d, base)
}

// factor returns base * U(min, max).
func (j *jitter) factor(base time.Duration, min, max float64) time.Duration {
	return time.Duration(float64(base) * (min + j.rng.Float64()*(max-min)))
}

func clampPause(p, base time.Duration) time.Duration {
	if floor := time.Duration(float64(base) * pauseFloorFactor); p < floor {
		return floor
	}
	if ceil := time.Duration(float64(base) * pauseCeilFactor); p > ceil {
		return ceil
	}
	return p
}

// Tuning constants, fitted to captured typing. Timing factors are multiples of
// the base per-key delay, so they scale with #$ delay.
const (
	jitterSigma = 0.5 // lognormal sigma of per-key timing

	// boundary / digraph multipliers (applied to the pause before a key, by
	// what preceded it)
	spaceFactor      = 2.8 // after a space (word boundary)
	punctFactor      = 2.7 // after punctuation
	digitFactor      = 1.2 // after a digit
	doubleFactor     = 1.15
	altHandFactor    = 0.9 // letter->letter, hands alternate (fast)
	sameHandFactor   = 1.0 // letter->letter, same hand, different finger
	sameFingerFactor = 1.3 // letter->letter, same finger (slow reach)

	// Floor on the faded digraph multiplier. The sub-1 factors above go
	// negative past a scale of ~10, which would make a big --jitter type
	// faster and flatter -- the opposite of what it asks for.
	minTransMult = 0.1

	hesitationProb = 0.08 // chance of a thinking stall per key
	hesitationMin  = 2.5
	hesitationMax  = 4.0

	// Hard bounds on a pause, either side of the base delay: nothing reads as a
	// hang, and nothing arrives instantly. Both matter most at a big --jitter,
	// where the lognormal spans orders of magnitude.
	pauseFloorFactor = 0.1
	pauseCeilFactor  = 25.0
)

// transMult is the unscaled timing multiplier for typing ch right after prev.
func transMult(prev, ch rune) float64 {
	switch {
	case prev == ' ':
		return spaceFactor
	case isPunct(prev):
		return punctFactor
	case isDigit(prev):
		return digitFactor
	case isLetter(prev) && isLetter(ch):
		lp, lc := lower(prev), lower(ch)
		if lp == lc {
			return doubleFactor
		}
		hp, okp := handOf[lp]
		hc, okc := handOf[lc]
		if okp && okc {
			switch {
			case hp != hc:
				return altHandFactor
			case fingerOf[lp] == fingerOf[lc]:
				return sameFingerFactor
			default:
				return sameHandFactor
			}
		}
		return sameHandFactor
	default:
		return 1.0
	}
}

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func isPunct(r rune) bool {
	return strings.ContainsRune(".,;:!?/-_=+\"'`~|&$*(){}[]<>@#%^\\", r)
}

func lower(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + 32
	}
	return r
}

// QWERTY hand/finger maps (lowercase). Same finger = slow reach; different hand
// = fast alternation. Populated in init from finger rows.
var (
	handOf   = map[rune]byte{}
	fingerOf = map[rune]byte{}
)

func init() {
	rows := []struct {
		finger byte
		keys   string
	}{
		{'0', "qaz"}, {'1', "wsx"}, {'2', "edc"}, {'3', "rfvtgb"}, // left pinky..index
		{'4', "yhnujm"}, {'5', "ik"}, {'6', "ol"}, {'7', "p"}, // right index..pinky
	}
	for _, row := range rows {
		hand := byte('L')
		if row.finger >= '4' {
			hand = 'R'
		}
		for _, c := range row.keys {
			fingerOf[c] = row.finger
			handOf[c] = hand
		}
	}
}
