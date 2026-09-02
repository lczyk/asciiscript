//go:build !windows

package main

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

// recorder stands in for the pty and the clock, so typing can be replayed
// without a terminal and its keystroke/pause interleaving inspected.
type recorder struct {
	events  []string
	err     error
	onWrite func(string) // stands in for the recorded shell reacting to input
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	r.events = append(r.events, "w:"+string(p))
	if r.onWrite != nil {
		r.onWrite(string(p))
	}
	return len(p), nil
}

func (r *recorder) Close() error { return nil }

func (r *recorder) sleep(d time.Duration) error {
	r.events = append(r.events, "s:"+d.String())
	return nil
}

// newTestSession is a session wired to a recorder instead of a pty and a
// clock, warnings captured, typing with the real timing model at a fixed seed.
// The shell's first prompt is already on screen, as it is when a take starts.
func newTestSession(t *testing.T) (*session, *recorder) {
	t.Helper()
	s := newSession()
	rec := &recorder{}
	s.pty = rec
	s.sleep = rec.sleep
	s.jitter = newJitter(1, 7)
	s.warn = &bytes.Buffer{}
	s.mon.marks = 1
	return s, rec
}

// cmd is a one-line command typed at zero delay, so what a recorder sees is
// the structure of the typing and nothing else.
func cmd(line string) command { return command{lines: []string{line}} }

// warnings is everything the session has reported to the user, the recorded
// session untouched.
func warnings(s *session) string { return s.warn.(*bytes.Buffer).String() }

// typedLines reassembles the keystrokes a recorder saw into whole lines, so a
// test can assert on what was typed without minding the per-key writes.
func typedLines(r *recorder) []string {
	var typed strings.Builder
	for _, e := range r.events {
		if after, ok := strings.CutPrefix(e, "w:"); ok {
			typed.WriteString(after)
		}
	}
	lines := strings.SplitAfter(typed.String(), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

func TestTermSizeUsesWhatWasAskedFor(t *testing.T) {
	cols, rows := termSize(&options{Cols: 100, Rows: 30}, false)
	assert.Equal(t, cols, uint16(100))
	assert.Equal(t, rows, uint16(30))

	cols, rows = termSize(&options{Cols: 100, Rows: 30}, true)
	assert.Equal(t, cols, uint16(100))
	assert.Equal(t, rows, uint16(30))
}

// A dimension left at zero is 80x24's, whatever screen the take is made on,
// without disturbing the one that was given.
func TestTermSizeDefaultsTo80x24(t *testing.T) {
	cols, rows := termSize(&options{Cols: 100}, false)
	assert.Equal(t, cols, uint16(100))
	assert.Equal(t, rows, uint16(24))

	cols, rows = termSize(&options{Rows: 30}, false)
	assert.Equal(t, cols, uint16(80))
	assert.Equal(t, rows, uint16(30))

	cols, rows = termSize(&options{}, false)
	assert.Equal(t, cols, uint16(80))
	assert.Equal(t, rows, uint16(24))
}

// With a handover the current terminal's size fills in -- or 80x24 again when
// there isn't one, as under go test.
func TestTermSizeFollowsTheTerminalForAHandover(t *testing.T) {
	cols, rows := termSize(&options{}, true)
	assert.That(t, cols > 0 && rows > 0, "both should be filled in")
}

func TestValidateAcceptsTheDefaults(t *testing.T) {
	assert.NoError(t, (&options{Speed: 1, Jitter: 1, CmdTimeout: 600000, ExitTimeout: 10000}).validate())
	assert.NoError(t, (&options{Speed: 2, Jitter: 0, IdleTimeLimit: 2.5, Cols: 65535, Rows: 1, CmdTimeout: 1, ExitTimeout: 1}).validate())
}

// Every flag that is a number has a range, and each is refused by name.
func TestValidateRejectsEachBadFlag(t *testing.T) {
	good := options{Speed: 1, Jitter: 1, CmdTimeout: 600000, ExitTimeout: 10000}
	for _, tc := range []struct {
		flag string
		set  func(*options)
	}{
		{"--cols", func(o *options) { o.Cols = -1 }},
		{"--cols", func(o *options) { o.Cols = 70000 }},
		{"--rows", func(o *options) { o.Rows = -1 }},
		{"--rows", func(o *options) { o.Rows = 65536 }},
		{"--speed", func(o *options) { o.Speed = 0 }},
		{"--speed", func(o *options) { o.Speed = -1 }},
		{"--jitter", func(o *options) { o.Jitter = -0.5 }},
		{"--idle-time-limit", func(o *options) { o.IdleTimeLimit = -1 }},
		{"--cmd-timeout", func(o *options) { o.CmdTimeout = 0 }},
		{"--exit-timeout", func(o *options) { o.ExitTimeout = -5 }},
	} {
		o := good
		tc.set(&o)
		assert.Error(t, o.validate(), tc.flag)
	}
}

func TestAsciinemaMajor(t *testing.T) {
	for _, tc := range []struct {
		out   string
		major int
		ok    bool
	}{
		{"asciinema 3.2.1\n", 3, true},
		{"asciinema 2.4.0", 2, true},
		{"asciinema 3.0.0-rc.1 (Rust)", 3, true},
		{"", 0, false},
		{"asciinema", 0, false},
	} {
		major, ok := asciinemaMajor(tc.out)
		assert.Equal(t, ok, tc.ok, tc.out)
		assert.Equal(t, major, tc.major, tc.out)
	}
}

// The README's Flags block is written by hand, so the one thing that can be
// checked is that every flag is in it.
func TestReadmeListsEveryFlag(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	assert.NoError(t, err)

	typ := reflect.TypeOf(options{})
	for i := range typ.NumField() {
		long := typ.Field(i).Tag.Get("long")
		if long == "" {
			continue
		}
		assert.ContainsString(t, string(readme), "--"+long)
	}
}
