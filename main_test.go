package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

// recorder stands in for the pty and the clock, so a shell.Run can be replayed
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

func newRecordedScript(t *testing.T, delay time.Duration) (*script, *recorder) {
	t.Helper()
	s, err := parseScript("")
	assert.NoError(t, err)
	rec := &recorder{}
	s.delay = delay
	s.pty = rec
	s.sleep = rec.sleep
	s.jitter = newJitter(1, 7)
	s.warn = &bytes.Buffer{}
	return s, rec
}

// newScriptFrom is newRecordedScript for a script with actual content, typing
// at zero delay so the recorded events are the structure and nothing else.
func newScriptFrom(t *testing.T, text string) (*script, *recorder) {
	t.Helper()
	s, err := parseScript(text)
	assert.NoError(t, err)
	rec := &recorder{}
	s.delay = 0
	s.pty = rec
	s.sleep = rec.sleep
	s.jitter = newJitter(0, 1)
	s.warn = &bytes.Buffer{}
	return s, rec
}

// warnings is everything the script has reported to the user, the recorded
// session untouched.
func warnings(s *script) string { return s.warn.(*bytes.Buffer).String() }

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
	cols, rows := termSize(&options{Cols: 100, Rows: 30})
	assert.Equal(t, cols, uint16(100))
	assert.Equal(t, rows, uint16(30))
}

// A dimension left at zero gets filled in from somewhere -- the real terminal,
// or 80x24 when there isn't one -- without disturbing the one that was given.
func TestTermSizeFillsInWhatWasNot(t *testing.T) {
	cols, rows := termSize(&options{Cols: 100})
	assert.Equal(t, cols, uint16(100))
	assert.That(t, rows > 0, "rows should be filled in")

	cols, rows = termSize(&options{Rows: 30})
	assert.Equal(t, rows, uint16(30))
	assert.That(t, cols > 0, "cols should be filled in")

	cols, rows = termSize(&options{})
	assert.That(t, cols > 0 && rows > 0, "both should be filled in")
}
