package main

import (
	"math"
	"math/rand"
	"strings"
	"time"
)

// human is the opt-in human-typing subsystem (enabled via --human). It turns a
// command line into a plan of keystrokes and pauses that reads as hand-typed
// rather than machine-uniform. Everything about "looking human" lives here; the
// executor just asks plan() for a line and replays the keystrokes.
//
// Timing (fitted to real captured typing) is the whole game -- no fabricated
// mistakes, so the typed text always matches the script:
//
//   - per-key pause = base * transition-mult * lognormal jitter, where
//     transition-mult captures word/punctuation boundaries and digraph physics
//     (alternating hands fast, same-finger slow).
//   - hesitation: the occasional thinking stall.
type human struct {
	rng    *rand.Rand
	jitter float64
}

// Tuning constants, fitted to captured typing. Timing factors are multiples of
// the base per-key delay, so they scale with `#$ delay`.
const (
	humanJitter = 0.5 // lognormal sigma of per-key timing

	// transition multipliers (pause before a key, by what preceded it)
	spaceFactor      = 2.8 // after a space (word boundary)
	punctFactor      = 2.7 // after punctuation
	digitFactor      = 1.2 // after a digit
	doubleFactor     = 1.15
	altHandFactor    = 0.9 // letter->letter, hands alternate (fast)
	sameHandFactor   = 1.0 // letter->letter, same hand, different finger
	sameFingerFactor = 1.3 // letter->letter, same finger (slow reach)

	hesitationProb = 0.08 // chance of a thinking stall per key
	hesitationMin  = 2.5
	hesitationMax  = 4.0

	pauseCeilFactor = 25.0 // hard cap so no gap reads as a hang
)

func newHuman(seed int64) *human {
	return &human{rng: rand.New(rand.NewSource(seed)), jitter: humanJitter}
}

// plan converts a line into keystrokes typed at the given base per-key delay.
// (keystroke is defined alongside the executor in main.go.)
func (h *human) plan(line string, base time.Duration) []keystroke {
	runes := []rune(line)
	ks := make([]keystroke, 0, len(runes))
	var prev rune
	for _, ch := range runes {
		ks = append(ks, keystroke{string(ch), h.pauseFor(prev, ch, base)})
		prev = ch
	}
	return ks
}

// pauseFor is the delay before typing ch given the char before it.
func (h *human) pauseFor(prev, ch rune, base time.Duration) time.Duration {
	p := float64(base) * transMult(prev, ch) * math.Exp(h.rng.NormFloat64()*h.jitter)
	d := time.Duration(p)
	if h.rng.Float64() < hesitationProb {
		d += h.factor(base, hesitationMin, hesitationMax)
	}
	return clampPause(d, base)
}

// factor returns base * U(min, max).
func (h *human) factor(base time.Duration, min, max float64) time.Duration {
	return time.Duration(float64(base) * (min + h.rng.Float64()*(max-min)))
}

func clampPause(p, base time.Duration) time.Duration {
	if p < 0 {
		return 0
	}
	if ceil := time.Duration(float64(base) * pauseCeilFactor); p > ceil {
		return ceil
	}
	return p
}

// transMult is the timing multiplier for typing ch right after prev.
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
