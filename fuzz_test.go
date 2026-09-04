//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Fuzz targets for the parts that take input asciiscript doesn't control: a
// user's script, and the bytes a recorded shell writes back. Each asserts a
// property rather than a value, so a failing input is a real defect and not a
// changed expectation. Seed corpora run as ordinary tests under `go test`;
// `make fuzz` explores past them.

// chunked reads at most n bytes at a time, standing in for a pty handing over
// whatever happened to be in the buffer.
type chunked struct {
	s string
	n int
}

func (c *chunked) Read(p []byte) (int, error) {
	if c.s == "" {
		return 0, io.EOF
	}
	n := min(min(c.n, len(p)), len(c.s))
	copy(p, c.s[:n])
	c.s = c.s[n:]
	return n, nil
}

const fuzzMarkerName = "fuzz-fixed-marker"

// Counting prompt markers has to be exact however the stream is cut up: the
// count is what every wait compares against, so one missed or double-counted
// marker either hangs a line until its timeout or types over a running command.
func FuzzMirrorTally(f *testing.F) {
	mark := promptMarkerFor(fuzzMarkerName)
	one := "\x1b]133;D;0;" + mark.probe + "\x07"

	f.Add("", 1)
	f.Add(one, 1)
	f.Add(one+one, 7)
	f.Add("prompt$ "+one+"\r\noutput\r\n"+one, 3)
	f.Add(one[:len(one)-1], 2) // an almost-marker
	f.Add(mark.probe[:len(mark.probe)-1]+"!", 1)
	f.Add("asciiscript=0123456789abcdee", 5) // one byte off the token
	f.Add(strings.Repeat(one, 9), 4096)
	f.Add(`\[\e]133;D;$?;`+mark.probe+`\a\]`, 1) // PS1 as the shell would echo it, unexpanded

	f.Fuzz(func(t *testing.T, stream string, chunk int) {
		if chunk < 1 || chunk > 1<<16 || len(stream) > 1<<16 {
			t.Skip()
		}
		m := &mirror{quiet: true, mark: mark}
		m.run(&chunked{s: stream, n: chunk})

		if got, want := m.marked(), len(mark.strip.FindAllStringIndex(stream, -1)); got != want {
			t.Fatalf("counted %d markers in %q at chunk size %d, want %d", got, stream, chunk, want)
		}
	})
}

// clean only ever removes whole marker sequences and terminal queries, so its
// output is the input with pieces cut out -- never reordered, never longer.
// Half a sequence left behind would swallow the live terminal's output.
func FuzzMirrorClean(f *testing.F) {
	mark := promptMarkerFor(fuzzMarkerName)
	one := "\x1b]133;D;0;" + mark.probe + "\x07"

	f.Add("")
	f.Add(one)
	f.Add("before" + one + "after")
	f.Add("\x1b[6nq\x1b]11;?\x07" + one)
	f.Add("\x1b]133;D;;" + mark.probe + "\x07") // no exit status
	f.Add(mark.probe)                           // a bare probe is text, not a marker

	f.Fuzz(func(t *testing.T, stream string) {
		if len(stream) > 1<<16 {
			t.Skip()
		}
		m := &mirror{quiet: true, mark: mark}
		out := string(m.clean([]byte(stream)))

		if len(out) > len(stream) {
			t.Fatalf("clean(%q) grew to %q", stream, out)
		}
		if !subsequence(out, stream) {
			t.Fatalf("clean(%q) = %q, which isn't the input with pieces removed", stream, out)
		}
		// A bare probe in the output is just text -- a recording being cat'd,
		// say. Only a whole sequence is a marker, and none may survive, not
		// even one spliced together by the removal of what sat between.
		if m.mark.strip.MatchString(out) {
			t.Fatalf("clean(%q) = %q, which still carries a marker", stream, out)
		}
	})
}

func FuzzStripQueries(f *testing.F) {
	f.Add("")
	f.Add("plain text\r\n")
	f.Add("\x1b[6n")
	f.Add("a\x1b[0cb\x1b[?2026$pc")
	f.Add("\x1b]11;?\x07")
	f.Add("\x1b[6\x1b[6nn") // stripping the middle leaves a whole query behind

	f.Fuzz(func(t *testing.T, stream string) {
		if len(stream) > 1<<16 {
			t.Skip()
		}
		out := string(stripQueries([]byte(stream)))
		if len(out) > len(stream) {
			t.Fatalf("stripQueries(%q) grew to %q", stream, out)
		}
		if !subsequence(out, stream) {
			t.Fatalf("stripQueries(%q) = %q, which isn't the input with pieces removed", stream, out)
		}
	})
}

// Whatever a script says, parsing it either fails with one of the errors the
// tool knows how to report, or yields lines that are the script's own -- in
// order, each ending in the newline that runs it. A parser that dropped, split
// or reordered a line would type something the author never wrote.
func FuzzParseScript(f *testing.F) {
	f.Add("echo hi\n")
	f.Add("#$ delay 10\n#$ pause 20\n#$ handover\nnano f\n")
	f.Add("echo one \\\n  two\n")
	f.Add("echo \"a\nb\"\n")
	f.Add("echo hi\n#$ pause 100\n")
	f.Add("echo hi\n#$ delay 100\n")
	f.Add("cat <<'EOF'\nbody\nEOF\necho after\n")
	f.Add("cat <<-EOF\n\tbody\n\tEOF\n")
	f.Add("echo \"a << b\"\n\n\necho c\n")
	f.Add("#$ bogus\n")
	f.Add("cat <<EOF\n") // heredoc that never terminates
	f.Add("cat <<END-OF-FILE\nbody\nEND-OF-FILE\n")
	f.Add("cat <<\\EOF\nbody\nEOF\n")
	f.Add("cat <<A <<B\nbody a\nA\nbody b\nB\n")
	f.Add("echo $'don\\'t stop'\n")
	f.Add("echo a;#comment\n")
	f.Add("#$ delay -5\na\n")
	f.Add("#$ delay 9223372036854775807\na\n")

	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 1<<16 {
			t.Skip()
		}
		s, err := parseScript(text)
		if err != nil {
			switch {
			case strings.Contains(err.Error(), errUnknownCtrl.Error()),
				strings.Contains(err.Error(), errNoArgs.Error()),
				strings.Contains(err.Error(), errBadArg.Error()),
				strings.Contains(err.Error(), errArgRange.Error()),
				strings.Contains(err.Error(), errDangling.Error()),
				strings.Contains(err.Error(), errUnterminated.Error()):
				return
			}
			t.Fatalf("parseScript(%q) failed with an unreportable error: %v", text, err)
		}

		lines := strings.Split(text, "\n")
		at := 0
		for i, c := range s.commands {
			if len(c.lines) == 0 {
				t.Fatalf("parseScript(%q) command %d has no lines", text, i)
			}
			for _, want := range c.lines {
				for at < len(lines) && lines[at] != want {
					at++
				}
				if at == len(lines) {
					t.Fatalf("parseScript(%q) command %d has %q, which is not a line of the script (or is out of order)", text, i, want)
				}
				at++
			}
		}
	})
}

// The timing model may only shape the gaps: a recording always types the script
// exactly, and no gap is either instant or a hang.
func FuzzJitterPlan(f *testing.F) {
	f.Add("echo hi\n", 1.0, 40)
	f.Add("", 0.0, 40)
	f.Add("the quick brown fox\n", 20.0, 1)
	f.Add("a\n", 1e6, 40)
	f.Add("\xd7", 0.0, 40) // a byte that is not valid UTF-8
	f.Add("  \t\n", 0.5, 1000)
	f.Add("unicode: \u00e9\u00fc\u4e2d\n", 2.0, 40)

	f.Fuzz(func(t *testing.T, line string, scale float64, delayMS int) {
		if len(line) > 4096 || delayMS < 1 || delayMS > 60000 {
			t.Skip()
		}
		if math.IsNaN(scale) || math.IsInf(scale, 0) || scale < 0 || scale > 1e9 {
			t.Skip() // --jitter rejects negatives; the rest is beyond any real dial
		}
		base := time.Duration(delayMS) * time.Millisecond
		floor := time.Duration(float64(base) * pauseFloorFactor)
		ceil := time.Duration(float64(base) * pauseCeilFactor)

		ks := newJitter(scale, 12345).plan(line, base, 0)
		if got := replay(ks); got != line {
			t.Fatalf("plan(%q) types %q", line, got)
		}
		for i, k := range ks {
			if k.pause < floor || k.pause > ceil {
				t.Fatalf("plan(%q) at scale %g: keystroke %d (%q) pauses %s, outside [%s, %s]",
					line, scale, i, k.data, k.pause, floor, ceil)
			}
		}
	})
}

// subsequence reports whether small is large with pieces cut out, order kept.
func subsequence(small, large string) bool {
	i := 0
	for j := 0; j < len(large) && i < len(small); j++ {
		if small[i] == large[j] {
			i++
		}
	}
	return i == len(small)
}

// A recording must not depend on how the pty happened to chunk its output:
// feeding the same bytes through in one piece or in small pieces has to
// reassemble to the same text.
func FuzzCastChunking(f *testing.F) {
	f.Add([]byte{}, 1)
	f.Add([]byte("hello, world\n"), 3)
	f.Add([]byte("caf\xc3\xa9"), 1)           // e-acute, a two-byte rune
	f.Add([]byte("\xe4\xb8\xad"), 2)          // the CJK ideograph U+4E2D, a three-byte rune
	f.Add([]byte("\xf0\x9f\x98\x80"), 3)      // an emoji, a four-byte rune
	f.Add(bytes.Repeat([]byte{0xff}, 3), 1)   // a run of invalid bytes
	f.Add([]byte{0xe4, 0xb8}, 1)              // truncated at the very end
	f.Add(append([]byte{0xe4, 0xb8}, 'A'), 1) // truncated, then ascii

	f.Fuzz(func(t *testing.T, stream []byte, chunk int) {
		if chunk < 1 || chunk > 1<<16 || len(stream) > 1<<16 {
			t.Skip()
		}

		one := castRecording(t, stream, len(stream)+1)
		pieces := castRecording(t, stream, chunk)

		if got, want := outputData(t, pieces), outputData(t, one); got != want {
			t.Fatalf("chunk size %d decoded to %q, want %q", chunk, got, want)
		}
		// Valid UTF-8 goes into the file as it came, whatever the chunking.
		if got := outputData(t, one); utf8.Valid(stream) && got != string(stream) {
			t.Fatalf("decoded to %q, want the input %q", got, stream)
		}
		for _, l := range castLines(t, pieces) {
			if !utf8.ValidString(l.data) {
				t.Fatalf("chunk size %d: %q is not valid UTF-8", chunk, l.data)
			}
		}
		for _, line := range strings.Split(strings.TrimSpace(string(pieces)), "\n") {
			if !json.Valid([]byte(line)) {
				t.Fatalf("chunk size %d: line %q is not valid JSON", chunk, line)
			}
		}
	})
}

// The carry that keeps quantised intervals tracking the real clock must hold
// for any sequence of steps, not just the evenly spaced ones the unit tests use.
func FuzzCastIntervals(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add([]byte{1, 1, 1})
	f.Add([]byte{255, 0, 255, 0})
	f.Add(bytes.Repeat([]byte{13}, 50)) // ~1.3ms steps, like the 1000-event unit test
	f.Add([]byte{255, 255, 255})

	f.Fuzz(func(t *testing.T, steps []byte) {
		if len(steps) > 4096 {
			t.Skip()
		}
		deltas := make([]time.Duration, len(steps))
		for i, b := range steps {
			deltas[i] = time.Duration(b) * 100 * time.Microsecond
		}

		var buf bytes.Buffer
		c, err := newCastWriter(&buf, minimalHeader, clockFrom(time.Now(), deltas...))
		if err != nil {
			t.Fatalf("newCastWriter: %v", err)
		}
		for range steps {
			if err := c.marker("x"); err != nil {
				t.Fatalf("marker: %v", err)
			}
		}
		if err := c.close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		gaps := castGaps(t, buf.Bytes())
		var sumMS, trueMS float64
		for i, gap := range gaps {
			if gap < 0 {
				t.Fatalf("negative interval %v at event %d", gap, i)
			}
			ms := gap * 1000
			frac := ms - math.Trunc(ms)
			if frac > 1e-6 && frac < 1-1e-6 {
				t.Fatalf("interval %vs at event %d is not a whole millisecond", gap, i)
			}
			sumMS += ms
			trueMS += float64(deltas[i]) / float64(time.Millisecond)
			// The float64 round trip through JSON text adds noise far below
			// a millisecond; the epsilon absorbs that, not the property.
			if math.Abs(sumMS-trueMS) > 0.5+1e-6 {
				t.Fatalf("running sum %.4fms strayed from true %.4fms by more than 0.5ms at event %d", sumMS, trueMS, i)
			}
		}
	})
}

// appendJSONString must escape whatever sanitiseUTF8 hands it into a string
// encoding/json can parse back unchanged, with no raw control byte loose in
// the line for a terminal to act on.
func FuzzCastEscape(f *testing.F) {
	f.Add("")
	f.Add("hello\n\t\r")
	f.Add("quoted \" and \\backslash\\")
	f.Add("<script>&amp;</script>")
	f.Add("unicode: \u00e9\u4e2d\U0001f600")
	f.Add("\xff\x80") // not valid UTF-8

	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 1<<16 {
			t.Skip()
		}
		clean := sanitiseUTF8([]byte(s))
		line := appendJSONString(nil, clean)

		var decoded string
		if err := json.Unmarshal(line, &decoded); err != nil {
			t.Fatalf("appendJSONString(%q) = %q, which encoding/json won't parse: %v", clean, line, err)
		}
		if decoded != clean {
			t.Fatalf("appendJSONString(%q) round-tripped to %q", clean, decoded)
		}
		for _, b := range line {
			if b < 0x20 {
				t.Fatalf("appendJSONString(%q) = %q, which has a raw control byte", clean, line)
			}
		}
	})
}
