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
// It's a small state machine, with timing and probabilities fitted to real
// captured typing (see cmd/typer):
//
//   - timing:      per-key pause = base * transition-mult * lognormal jitter,
//     where transition-mult captures word/punctuation boundaries and
//     digraph physics (alternating hands fast, same-finger slow).
//   - hesitation:  the occasional thinking stall.
//   - mistakes:    a typo (wrong neighbour key) or an omission (a skipped char).
//     After a mistake you often type on a few keys before noticing,
//     pause, then correct -- by backspacing if the slip is near, or
//     arrowing back and fixing in place if it's further away.
//
// Invariant: replaying a plan through a line editor (insert at cursor, \x7f
// erases, arrows move) reproduces the original line exactly.
type human struct {
	rng    *rand.Rand
	jitter float64
}

// keystroke tokens that aren't literal text. Written to the pty, these drive
// bash's line editor; in tests they drive the replay editor.
const (
	keyBackspace = "\x7f"
	keyLeft      = "\x1b[D"
	keyRight     = "\x1b[C"
)

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

	typoProb         = 0.03 // chance of a mistake per letter (demo default; real ~0.06)
	substitutionProb = 0.6  // of mistakes, this many are typos; the rest omissions
	overshootP       = 0.6  // geometric: P(type-on = k) = (1-p)^k * p
	overshootCap     = 6
	backspaceMax     = 2 // slip within this many keys -> backspace; further -> arrow back

	noticeMin = 3.0 // "notice the mistake" pause before correcting
	noticeMax = 4.5
	fastMin   = 0.5 // backspaces / arrow presses are quick and steady
	fastMax   = 0.9

	pauseCeilFactor = 25.0 // hard cap so no gap reads as a hang
)

func newHuman(seed int64) *human {
	return &human{rng: rand.New(rand.NewSource(seed)), jitter: humanJitter}
}

// plan converts a line into keystrokes typed at the given base per-key delay.
// (keystroke is defined alongside the executor in main.go.)
func (h *human) plan(line string, base time.Duration) []keystroke {
	runes := []rune(line)
	var ks []keystroke
	var prev rune
	i := 0
	for i < len(runes) {
		ch := runes[i]
		if isLetter(ch) && h.rng.Float64() < typoProb {
			if h.rng.Float64() < substitutionProb {
				i, prev = h.substitution(&ks, runes, i, prev, base)
			} else {
				i, prev = h.omission(&ks, runes, i, prev, base)
			}
			continue
		}
		ks = append(ks, keystroke{string(ch), h.pauseFor(prev, ch, base)})
		prev = ch
		i++
	}
	return ks
}

// substitution types a wrong neighbour key in place of runes[i], possibly types
// on a few correct keys, then corrects. Returns the next index and prev rune.
func (h *human) substitution(ks *[]keystroke, runes []rune, i int, prev rune, base time.Duration) (int, rune) {
	ch := runes[i]
	wrong, ok := h.neighbour(ch)
	if !ok {
		*ks = append(*ks, keystroke{string(ch), h.pauseFor(prev, ch, base)})
		return i + 1, ch
	}
	n := h.overshoot(0, len(runes)-1-i)

	// wrong key, then n correct keys typed on obliviously
	*ks = append(*ks, keystroke{string(wrong), h.pauseFor(prev, ch, base)})
	last := wrong
	for k := 1; k <= n; k++ {
		c := runes[i+k]
		*ks = append(*ks, keystroke{string(c), h.pauseFor(last, c, base)})
		last = c
	}

	before := runeAt(runes, i-1)
	if n <= backspaceMax {
		// backspace the slip (n typed-on + the wrong key), then retype
		h.emitRepeat(ks, keyBackspace, n+1, base)
		h.retype(ks, runes, i, i+n, before, base)
	} else {
		// arrow back to just after the wrong key, delete it, insert the right
		// one, arrow back to the end
		h.emitRepeat(ks, keyLeft, n, base)
		*ks = append(*ks, keystroke{keyBackspace, h.fastPause(base)})
		*ks = append(*ks, keystroke{string(ch), h.pauseFor(before, ch, base)})
		h.emitRepeat(ks, keyRight, n, base)
	}
	return i + n + 1, runes[i+n]
}

// omission skips runes[i], types on a few keys, then goes back and inserts the
// missing char.
func (h *human) omission(ks *[]keystroke, runes []rune, i int, prev rune, base time.Duration) (int, rune) {
	maxOver := len(runes) - 1 - i
	if maxOver < 1 {
		ch := runes[i]
		*ks = append(*ks, keystroke{string(ch), h.pauseFor(prev, ch, base)})
		return i + 1, ch
	}
	n := h.overshoot(1, maxOver)

	// skip runes[i]; type the n following chars
	last := prev
	for k := 1; k <= n; k++ {
		c := runes[i+k]
		*ks = append(*ks, keystroke{string(c), h.pauseFor(last, c, base)})
		last = c
	}

	before := runeAt(runes, i-1)
	if n <= backspaceMax {
		// backspace the typed-on chars, then retype from the missing one
		h.emitRepeat(ks, keyBackspace, n, base)
		h.retype(ks, runes, i, i+n, before, base)
	} else {
		// arrow back to the gap, insert the missing char, arrow back to the end
		h.emitRepeat(ks, keyLeft, n, base)
		*ks = append(*ks, keystroke{string(runes[i]), h.pauseFor(before, runes[i], base)})
		h.emitRepeat(ks, keyRight, n, base)
	}
	return i + n + 1, runes[i+n]
}

// retype types runes[from..to] inclusive, seeding the digraph timing with the
// character already before `from`.
func (h *human) retype(ks *[]keystroke, runes []rune, from, to int, before rune, base time.Duration) {
	prev := before
	for k := from; k <= to; k++ {
		c := runes[k]
		*ks = append(*ks, keystroke{string(c), h.pauseFor(prev, c, base)})
		prev = c
	}
}

// emitRepeat appends the same key count times. The first carries the "notice"
// pause (the beat before you start correcting); the rest are quick.
func (h *human) emitRepeat(ks *[]keystroke, key string, count int, base time.Duration) {
	for k := 0; k < count; k++ {
		pause := h.fastPause(base)
		if k == 0 {
			pause = h.noticePause(base)
		}
		*ks = append(*ks, keystroke{key, pause})
	}
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

func (h *human) noticePause(base time.Duration) time.Duration {
	return clampPause(h.factor(base, noticeMin, noticeMax), base)
}

func (h *human) fastPause(base time.Duration) time.Duration {
	return clampPause(h.factor(base, fastMin, fastMax), base)
}

// factor returns base * U(min, max).
func (h *human) factor(base time.Duration, min, max float64) time.Duration {
	return time.Duration(float64(base) * (min + h.rng.Float64()*(max-min)))
}

// overshoot draws how many keys are typed past a mistake before it's noticed:
// geometric with parameter overshootP, clamped to [minN, min(maxN, cap)].
func (h *human) overshoot(minN, maxN int) int {
	n := minN
	for n < overshootCap && h.rng.Float64() > overshootP {
		n++
	}
	if maxN < minN {
		maxN = minN
	}
	if n > maxN {
		n = maxN
	}
	return n
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

func runeAt(runes []rune, i int) rune {
	if i < 0 || i >= len(runes) {
		return 0
	}
	return runes[i]
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

var qwertyRows = []string{"qwertyuiop", "asdfghjkl", "zxcvbnm"}

// neighbour returns an adjacent key on the same QWERTY row (case preserved),
// used to fabricate a believable typo. ok is false for keys we don't map.
// The caller guarantees r is an ASCII letter.
func (h *human) neighbour(r rune) (rune, bool) {
	l := lower(r)
	for _, row := range qwertyRows {
		idx := strings.IndexRune(row, l)
		if idx < 0 {
			continue
		}
		var cand []byte
		if idx > 0 {
			cand = append(cand, row[idx-1])
		}
		if idx+1 < len(row) {
			cand = append(cand, row[idx+1])
		}
		if len(cand) == 0 {
			return 0, false
		}
		n := rune(cand[h.rng.Intn(len(cand))])
		if r < 'a' { // preserve original case
			n -= 32
		}
		return n, true
	}
	return 0, false
}
