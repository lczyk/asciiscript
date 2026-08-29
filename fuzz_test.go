package main

import (
	"io"
	"math"
	"strings"
	"testing"
	"time"
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

	f.Fuzz(func(t *testing.T, stream string, chunk int) {
		if chunk < 1 || chunk > 1<<16 || len(stream) > 1<<16 {
			t.Skip()
		}
		m := &mirror{quiet: true, mark: mark}
		m.run(&chunked{s: stream, n: chunk})

		if got, want := m.marked(), strings.Count(stream, mark.probe); got != want {
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
