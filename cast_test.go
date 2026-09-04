//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

var minimalHeader = castHeader{Term: castTerm{Cols: 80, Rows: 24}}

// tickingClock starts at a fixed instant and advances by step on every call,
// standing in for the real clock's calls landing evenly apart. The first call
// (newCastWriter's epoch) returns the base instant.
func tickingClock(step time.Duration) func() time.Time {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time {
		t := base.Add(time.Duration(n) * step)
		n++
		return t
	}
}

// clockFrom returns a clock that yields base on the first call (the epoch),
// then base plus the running sum of deltas on each call after -- so deltas[i]
// is the gap between event i and event i+1 as far as the writer can tell.
func clockFrom(base time.Time, deltas ...time.Duration) func() time.Time {
	times := []time.Time{base}
	for _, d := range deltas {
		times = append(times, times[len(times)-1].Add(d))
	}
	i := 0
	return func() time.Time {
		t := times[i]
		i++
		return t
	}
}

// castLine is one decoded event: its code and already-unescaped data.
type castLine struct{ kind, data string }

// castLines decodes every event line of a recording, skipping the header.
func castLines(t *testing.T, b []byte) []castLine {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	out := make([]castLine, 0, len(lines)-1)
	for _, line := range lines[1:] {
		var raw []any
		assert.NoError(t, json.Unmarshal([]byte(line), &raw), line)
		assert.Len(t, raw, 3, line)
		out = append(out, castLine{kind: raw[1].(string), data: raw[2].(string)})
	}
	return out
}

// castGaps returns the interval field of every event line, in order.
func castGaps(t *testing.T, b []byte) []float64 {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	gaps := make([]float64, 0, len(lines)-1)
	for _, line := range lines[1:] {
		var raw []any
		assert.NoError(t, json.Unmarshal([]byte(line), &raw), line)
		gaps = append(gaps, raw[0].(float64))
	}
	return gaps
}

// outputData concatenates the decoded data of every "o" event -- what a
// viewer would see printed.
func outputData(t *testing.T, b []byte) string {
	t.Helper()
	var out strings.Builder
	for _, l := range castLines(t, b) {
		if l.kind == "o" {
			out.WriteString(l.data)
		}
	}
	return out.String()
}

// jsonKeys returns a JSON object line's top-level keys, in the order they
// appear in the bytes.
func jsonKeys(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	tok, err := dec.Token()
	assert.NoError(t, err)
	assert.Equal(t, fmt.Sprint(tok), "{")

	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		assert.NoError(t, err)
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		assert.NoError(t, dec.Decode(&skip))
	}
	return keys
}

// castRecording writes stream through a castWriter in pieces of at most
// chunk bytes, exits with status 0, and returns the recording's bytes.
func castRecording(t *testing.T, stream []byte, chunk int) []byte {
	t.Helper()
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	for len(stream) > 0 {
		n := min(chunk, len(stream))
		assert.NoError(t, c.output(stream[:n]))
		stream = stream[n:]
	}
	assert.NoError(t, c.exit(0))
	assert.NoError(t, c.close())
	return buf.Bytes()
}

// errAfterWriter accepts up to n bytes across every Write call, then fails
// every write after (including a call that spans the boundary) with err.
type errAfterWriter struct {
	buf bytes.Buffer
	n   int
	err error
}

func (w *errAfterWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, w.err
	}
	take := len(p)
	if take > w.n {
		take = w.n
	}
	w.buf.Write(p[:take])
	w.n -= take
	if take < len(p) {
		return take, w.err
	}
	return take, nil
}

var errBoom = errors.New("boom")

// --- header ---

func TestCastHeaderIsOneJSONLine(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, castHeader{
		Version:       99, // should be forced to 3
		Term:          castTerm{Cols: 80, Rows: 24, Type: "xterm-256color"},
		Timestamp:     1700000000,
		IdleTimeLimit: 2.5,
		Command:       "bash",
		Title:         "demo",
		Env:           &castEnv{Shell: "/bin/bash"},
	}, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.close())

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	assert.Len(t, lines, 1)
	line := lines[0]
	assert.That(t, strings.HasPrefix(line, "{"), "header should start with {")

	var generic map[string]any
	assert.NoError(t, json.Unmarshal([]byte(line), &generic))

	assert.EqualArrays(t, jsonKeys(t, line),
		[]string{"version", "term", "timestamp", "idle_time_limit", "command", "title", "env"})

	var h castHeader
	assert.NoError(t, json.Unmarshal([]byte(line), &h))
	assert.Equal(t, h.Version, 3)
}

func TestCastHeaderMinimalIsExact(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.close())
	assert.Equal(t, buf.String(), "{\"version\":3,\"term\":{\"cols\":80,\"rows\":24}}\n")
}

// Version is asciicast v3's, not the caller's -- newCastWriter is the only
// place that decides the file format it writes.
func TestCastHeaderVersionIsAlwaysThree(t *testing.T) {
	for _, v := range []int{0, 1, 2, 99, -5} {
		var buf bytes.Buffer
		c, err := newCastWriter(&buf, castHeader{Version: v, Term: castTerm{Cols: 80, Rows: 24}}, tickingClock(time.Millisecond))
		assert.NoError(t, err)
		assert.NoError(t, c.close())
		assert.ContainsString(t, buf.String(), `"version":3`, v)
	}
}

func TestCastHeaderOmitsZeroOptionalFields(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, castHeader{Term: castTerm{Cols: 80, Rows: 24}}, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.close())

	line := strings.TrimSpace(buf.String())
	for _, key := range []string{"timestamp", "idle_time_limit", "command", "title", "env", "type"} {
		assert.That(t, !strings.Contains(line, `"`+key+`"`), fmt.Sprintf("zero %s should be omitted", key))
	}
}

// asciinema title bars render this raw, so it must survive with the
// characters JSON usually escapes for HTML intact.
func TestCastHeaderTitleRoundTrips(t *testing.T) {
	const title = `<script>&"quoted"</script>`
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, castHeader{Term: castTerm{Cols: 80, Rows: 24}, Title: title}, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.close())

	line := strings.TrimSpace(buf.String())
	assert.ContainsString(t, line, "<script>&")

	var h castHeader
	assert.NoError(t, json.Unmarshal([]byte(line), &h))
	assert.Equal(t, h.Title, title)
}

// --- event shape ---

func TestCastEventLineShape(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(40*time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.output([]byte("hi")))
	assert.NoError(t, c.input([]byte("x")))
	assert.NoError(t, c.marker("m"))
	assert.NoError(t, c.resize(101, 42))
	assert.NoError(t, c.exit(137))
	assert.NoError(t, c.close())

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	assert.Equal(t, lines[0], `{"version":3,"term":{"cols":80,"rows":24}}`)
	want := []string{
		`[0.040, "o", "hi"]`,
		`[0.040, "i", "x"]`,
		`[0.040, "m", "m"]`,
		`[0.040, "r", "101x42"]`,
		`[0.040, "x", "137"]`,
	}
	assert.EqualArrays(t, lines[1:], want)
}

func TestCastMarkerEmptyLabelIsWrittenNotSkipped(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.marker(""))
	assert.NoError(t, c.close())
	assert.ContainsString(t, buf.String(), `, "m", ""]`)
}

// --- escaping ---

func TestCastAppendJSONStringEscapesExactBytes(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"ESC", "\x1b", `"\u001b"`},
		{"BEL", "\x07", `"\u0007"`},
		{"NUL", "\x00", `"\u0000"`},
		{"newline", "\n", `"\n"`},
		{"CR", "\r", `"\r"`},
		{"tab", "\t", `"\t"`},
		{"quote", `"`, `"\""`},
		{"backslash", `\`, `"\\"`},
		{"DEL", "\x7f", "\"\x7f\""},
		{"angle brackets", "<>", `"<>"`},
		{"ampersand", "&", `"&"`},
		{"slash", "/", `"/"`},
		{"two-byte rune", "\u00e9", "\"\u00e9\""},
		{"three-byte rune", "\u4e2d", "\"\u4e2d\""},
		{"four-byte rune", "\U0001F600", "\"\U0001F600\""},
	} {
		got := string(appendJSONString(nil, tc.in))
		assert.Equal(t, got, tc.want, tc.name)

		var decoded string
		assert.NoError(t, json.Unmarshal([]byte(got), &decoded), tc.name)
		assert.Equal(t, decoded, tc.in, tc.name)
	}
}

// --- UTF-8 reassembly across output() calls ---

func TestCastOutputReassemblesRuneSplitAcrossTwoCalls(t *testing.T) {
	for _, r := range []rune{'\u00e9', '\u4e2d', '\U0001F600'} {
		b := []byte(string(r))
		for k := 1; k < len(b); k++ {
			var buf bytes.Buffer
			c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Millisecond))
			assert.NoError(t, err)

			assert.NoError(t, c.output(b[:k]))
			assert.NoError(t, c.output(b[k:]))
			assert.NoError(t, c.close())

			lines := castLines(t, buf.Bytes())
			assert.Len(t, lines, 1, fmt.Sprintf("rune %q split at %d", r, k))
			assert.Equal(t, lines[0].kind, "o")
			assert.Equal(t, lines[0].data, string(r), fmt.Sprintf("rune %q split at %d", r, k))
		}
	}
}

func TestCastOutputReassemblesRuneSplitAcrossThreeCalls(t *testing.T) {
	r := '\U0001F600'
	b := []byte(string(r))
	for _, cuts := range [][2]int{{1, 1}, {1, 2}, {2, 1}} {
		var buf bytes.Buffer
		c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Millisecond))
		assert.NoError(t, err)

		a, mid, tail := b[:cuts[0]], b[cuts[0]:cuts[0]+cuts[1]], b[cuts[0]+cuts[1]:]
		assert.NoError(t, c.output(a))
		assert.NoError(t, c.output(mid))
		assert.NoError(t, c.output(tail))
		assert.NoError(t, c.close())

		lines := castLines(t, buf.Bytes())
		assert.Len(t, lines, 1, cuts)
		assert.Equal(t, lines[0].data, string(r), cuts)
	}
}

func TestCastOutputPartialSequenceWritesNoEvent(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.output([]byte{0xe4, 0xb8})) // half of the CJK ideograph U+4E2D, nothing more coming yet
	assert.NoError(t, c.close())

	// close() flushes the tail, so this proves the first call alone wrote
	// nothing by checking there is exactly the flushed event, not two.
	lines := castLines(t, buf.Bytes())
	assert.Len(t, lines, 1)
}

func TestCastOutputInvalidByteBecomesOneReplacementChar(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.output([]byte{0xff}))
	assert.NoError(t, c.close())

	lines := castLines(t, buf.Bytes())
	assert.Len(t, lines, 1)
	assert.Equal(t, lines[0].data, "\uFFFD")
}

func TestCastOutputTwoLoneContinuationBytesBecomeTwoReplacementChars(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.output([]byte{0x80, 0x81}))
	assert.NoError(t, c.close())

	lines := castLines(t, buf.Bytes())
	assert.Len(t, lines, 1)
	assert.Equal(t, lines[0].data, "\uFFFD\uFFFD")
}

// The truncated lead+continuation is one mistake (a maximal subpart), not
// two -- counting per byte would over-report how much was lost.
func TestCastOutputTruncatedThreeByteSequenceThenASCII(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.output([]byte{0xe4, 0xb8, 0x41}))
	assert.NoError(t, c.close())

	lines := castLines(t, buf.Bytes())
	assert.Len(t, lines, 1)
	assert.Equal(t, lines[0].data, "\uFFFDA")
}

func TestCastOutputTruncatedSequenceFlushedByExit(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.output([]byte{0xf0, 0x9f, 0x98})) // 3 of 4 bytes of an emoji
	assert.NoError(t, c.exit(0))
	assert.NoError(t, c.close())

	lines := castLines(t, buf.Bytes())
	assert.Len(t, lines, 2)
	assert.Equal(t, lines[0], castLine{"o", "\uFFFD"})
	assert.Equal(t, lines[1], castLine{"x", "0"})
}

func TestCastOutputTruncatedSequenceFlushedByClose(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.output([]byte{0xf0, 0x9f, 0x98}))
	assert.NoError(t, c.close())

	lines := castLines(t, buf.Bytes())
	assert.Len(t, lines, 1)
	assert.Equal(t, lines[0], castLine{"o", "\uFFFD"})
}

func TestCastOutputBadSecondByteSplitsIntoTwoReplacements(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.output([]byte{0xe0, 0x80}))
	assert.NoError(t, c.close())

	lines := castLines(t, buf.Bytes())
	assert.Len(t, lines, 1)
	assert.Equal(t, lines[0].data, "\uFFFD\uFFFD")
}

// --- intervals ---

func TestCastFirstIntervalIsFromConstructionNotFromZero(t *testing.T) {
	base := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, clockFrom(base, 250*time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.marker("x"))
	assert.NoError(t, c.close())

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	assert.Equal(t, lines[1], `[0.250, "m", "x"]`)
}

func TestCastEqualInstantsGiveZeroInterval(t *testing.T) {
	base := time.Now()
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, clockFrom(base, 10*time.Millisecond, 0))
	assert.NoError(t, err)
	assert.NoError(t, c.marker("a"))
	assert.NoError(t, c.marker("b"))
	assert.NoError(t, c.close())

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	assert.Equal(t, lines[1], `[0.010, "m", "a"]`)
	assert.Equal(t, lines[2], `[0.000, "m", "b"]`)
}

func TestCastBackwardsClockNeverProducesNegativeInterval(t *testing.T) {
	base := time.Now()
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, clockFrom(base, 100*time.Millisecond, -50*time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.marker("a"))
	assert.NoError(t, c.marker("b"))
	assert.NoError(t, c.close())

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	assert.Equal(t, lines[1], `[0.100, "m", "a"]`)
	assert.Equal(t, lines[2], `[0.000, "m", "b"]`, "a clock running backwards should floor at the previous event")
}

func TestCastThreeHourGapIsWrittenInSeconds(t *testing.T) {
	base := time.Now()
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, clockFrom(base, 3*time.Hour))
	assert.NoError(t, err)
	assert.NoError(t, c.marker("a"))
	assert.NoError(t, c.close())

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	assert.Equal(t, lines[1], `[10800.000, "m", "a"]`)
}

// Worked from quantise directly: corrected = d + carry, steps = (corrected +
// 500000ns)/1ms, carry = corrected - steps*1ms.
func TestCastSubMillisecondStepsCarryTheRoundingError(t *testing.T) {
	base := time.Now()
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, clockFrom(base, 400*time.Microsecond, 400*time.Microsecond, 200*time.Microsecond))
	assert.NoError(t, err)
	assert.NoError(t, c.marker("a"))
	assert.NoError(t, c.marker("b"))
	assert.NoError(t, c.marker("c"))
	assert.NoError(t, c.close())

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	assert.Equal(t, lines[1], `[0.000, "m", "a"]`)
	assert.Equal(t, lines[2], `[0.001, "m", "b"]`)
	assert.Equal(t, lines[3], `[0.000, "m", "c"]`)
}

// The error-diffusion property: quantising each interval to the millisecond
// and carrying the remainder forward keeps the written running total within
// half a millisecond of the real one, at every prefix -- not just at the end.
func TestCastErrorDiffusionKeepsRunningSumAccurate(t *testing.T) {
	const n = 1000
	step := 4 * time.Millisecond / 3 // 1.3333ms, doesn't divide evenly into 1ms

	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(step))
	assert.NoError(t, err)
	for range n {
		assert.NoError(t, c.marker("x"))
	}
	assert.NoError(t, c.close())

	gaps := castGaps(t, buf.Bytes())
	assert.Len(t, gaps, n)

	var sumMS, trueMS float64
	for i, gap := range gaps {
		sumMS += gap * 1000
		trueMS += float64(step) / float64(time.Millisecond)
		// The float64 round trip through JSON text adds noise far below a
		// millisecond; the epsilon absorbs that, not the property.
		assert.That(t, math.Abs(sumMS-trueMS) <= 0.5+1e-6,
			fmt.Sprintf("prefix %d: written sum %.4fms strayed from true %.4fms", i+1, sumMS, trueMS))
	}

	// A naive per-event round (no carry) would give 1000*round(1.3333ms) =
	// 1000ms, 333ms short of the true total -- proof the carry matters.
	naive := float64(n) * math.Round(float64(step)/float64(time.Millisecond))
	assert.That(t, math.Abs(naive-trueMS) > 0.5, "naive per-event rounding should have drifted")
}

// --- errors ---

func TestCastOutputFailsAndLatchesTheError(t *testing.T) {
	fw := &errAfterWriter{n: 0, err: errBoom}
	c, err := newCastWriter(fw, minimalHeader, tickingClock(time.Millisecond))
	assert.NoError(t, err) // the header is buffered, so it never touches fw

	big := bytes.Repeat([]byte("x"), 8192) // forces a real Write to fw
	err = c.output(big)
	assert.Error(t, err, errBoom)
	assert.Error(t, c.failed(), errBoom)
	assert.Equal(t, fw.buf.Len(), 0, "nothing should have reached the writer")

	err = c.marker("m")
	assert.Error(t, err, errBoom)
	assert.Equal(t, fw.buf.Len(), 0, "a call after the error should not write anything")

	err = c.close()
	assert.Error(t, err, errBoom)
}

func TestCastCloseReturnsUnderlyingWriteError(t *testing.T) {
	fw := &errAfterWriter{n: 20, err: errBoom} // enough for the header, not for what follows
	c, err := newCastWriter(fw, minimalHeader, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.marker("hello")) // buffered, doesn't touch fw yet

	err = c.close()
	assert.Error(t, err, errBoom)
	assert.Error(t, c.failed(), errBoom)

	// A second close must not attempt another write or change the error.
	assert.Error(t, c.close(), errBoom)
}

func TestCastCloseFlushesToTheUnderlyingWriter(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.output([]byte("hi")))
	assert.Equal(t, buf.Len(), 0, "bufio should be holding everything until close")

	assert.NoError(t, c.close())
	assert.That(t, buf.Len() > 0, "close should have flushed the recording")
}

// --- concurrency ---

func TestCastConcurrentCallsProduceWellFormedLines(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, minimalHeader, tickingClock(time.Microsecond))
	assert.NoError(t, err)

	const n = 200
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) { c.output([]byte(fmt.Sprintf("o%d", i))); done <- struct{}{} }(i)
		go func(i int) { c.input([]byte(fmt.Sprintf("i%d", i))); done <- struct{}{} }(i)
		go func(i int) { c.marker(fmt.Sprintf("m%d", i)); done <- struct{}{} }(i)
		go func(i int) { c.resize(uint16(i), uint16(i)); done <- struct{}{} }(i)
	}
	for i := 0; i < 4*n; i++ {
		<-done
	}
	assert.NoError(t, c.exit(0))
	assert.NoError(t, c.close())

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	assert.Equal(t, len(lines), 1+4*n+1, "header, every event, and the exit")
	for _, line := range lines[1:] {
		var raw []any
		assert.NoError(t, json.Unmarshal([]byte(line), &raw), line)
	}
}

// --- whole file ---

func TestCastWholeFileEventsInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.cast")
	f, err := os.Create(path)
	assert.NoError(t, err)

	c, err := newCastWriter(f, minimalHeader, tickingClock(40*time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.output([]byte("hi")))
	assert.NoError(t, c.output([]byte(" there")))
	assert.NoError(t, c.marker("checkpoint"))
	assert.NoError(t, c.resize(101, 42))
	assert.NoError(t, c.exit(0))
	assert.NoError(t, c.close())
	assert.NoError(t, f.Close())

	events := readCast(t, path)
	assert.Len(t, events, 5)
	kinds := make([]string, len(events))
	for i, e := range events {
		kinds[i] = e.kind
	}
	assert.EqualArrays(t, kinds, []string{"o", "o", "m", "r", "x"})
	assert.Equal(t, events[0].data, "hi")
	assert.Equal(t, events[1].data, " there")
	assert.Equal(t, events[2].data, "checkpoint")
	assert.Equal(t, events[3].data, "101x42")
	assert.Equal(t, events[4].data, "0")

	raw, err := os.ReadFile(path)
	assert.NoError(t, err)
	assert.That(t, strings.HasSuffix(string(raw), "\n"), "file should end with a newline")
	assert.That(t, !strings.Contains(string(raw), "\n\n"), "no blank lines")
	assert.Equal(t, strings.Count(string(raw), "\n"), 6, "header plus 5 events, one newline each")
}

// --- golden ---

// Computed by hand from the rules above: header line, then five events each
// 40ms apart per the fake clock.
func TestCastGoldenFile(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, castHeader{
		Term:    castTerm{Cols: 80, Rows: 24, Type: "xterm-256color"},
		Command: "bash",
		Title:   "demo",
	}, tickingClock(40*time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.output([]byte("hi")))
	assert.NoError(t, c.output([]byte(" there")))
	assert.NoError(t, c.marker("go"))
	assert.NoError(t, c.resize(101, 42))
	assert.NoError(t, c.exit(0))
	assert.NoError(t, c.close())

	want := `{"version":3,"term":{"cols":80,"rows":24,"type":"xterm-256color"},"command":"bash","title":"demo"}
[0.040, "o", "hi"]
[0.040, "o", " there"]
[0.040, "m", "go"]
[0.040, "r", "101x42"]
[0.040, "x", "0"]
`
	assert.Equal(t, buf.String(), want)
}

// The exit event is the last line: output that trickles in after it -- a
// drain that outlived its grace -- is dropped rather than written after x.
func TestCastNothingFollowsExit(t *testing.T) {
	var buf bytes.Buffer
	c, err := newCastWriter(&buf, castHeader{Term: castTerm{Cols: 80, Rows: 24}}, tickingClock(time.Millisecond))
	assert.NoError(t, err)
	assert.NoError(t, c.exit(0))
	assert.NoError(t, c.output([]byte("late")))
	assert.NoError(t, c.marker("late"))
	assert.NoError(t, c.exit(1))
	assert.NoError(t, c.close())

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	assert.Len(t, lines, 2)
	assert.ContainsString(t, lines[1], `"x"`)
}
