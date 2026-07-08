package main

import (
	"bufio"
	"io"
	"strings"
	"testing"

	"github.com/lczyk/assert"

	"github.com/christopher-dG/asciiscript/internal/typinglog"
)

func run(script, target string) (typinglog.Record, bool) {
	in := bufio.NewReader(strings.NewReader(script))
	return capture(in, io.Discard, target, 1, 1)
}

func kinds(rec typinglog.Record, k typinglog.Kind) int {
	n := 0
	for _, e := range rec.Events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

// Arrow back into the middle of the line and insert a missing character.
func TestCaptureArrowInsert(t *testing.T) {
	// target "test"; type "tst" (missing e), left twice, insert e -> "test".
	rec, quit := run("tst\x1b[D\x1b[De\r", "test")
	assert.That(t, !quit, "should not quit")
	assert.Equal(t, rec.Final, "test")
	assert.That(t, rec.Matched, "final matches target")
	assert.Equal(t, kinds(rec, typinglog.Left), 2)
	// the inserted 'e' lands where target wants it -> classified correct
	assert.That(t, kinds(rec, typinglog.Correct) >= 1, "insertion classified correct")
}

// Backspace removes the char before the cursor.
func TestCaptureBackspace(t *testing.T) {
	// target "ab"; type "az", backspace the z, type b -> "ab".
	rec, _ := run("az\x7fb\r", "ab")
	assert.Equal(t, rec.Final, "ab")
	assert.That(t, rec.Matched, "final matches target")
	assert.Equal(t, kinds(rec, typinglog.Backspace), 1)
	assert.Equal(t, kinds(rec, typinglog.Typo), 1) // the 'z'
}

// Forward-delete (ESC [ 3 ~) removes the char at the cursor.
func TestCaptureForwardDelete(t *testing.T) {
	// target "ab"; type "axb", home, right, delete the x -> "ab".
	rec, _ := run("axb\x1b[H\x1b[C\x1b[3~\r", "ab")
	assert.Equal(t, rec.Final, "ab")
	assert.That(t, rec.Matched, "final matches target")
	assert.Equal(t, kinds(rec, typinglog.Delete), 1)
	assert.Equal(t, kinds(rec, typinglog.Home), 1)
}

// Ctrl-C quits and reports it.
func TestCaptureQuit(t *testing.T) {
	_, quit := run("ab\x03", "abc")
	assert.That(t, quit, "ctrl-c should quit")
}

// Every event carries non-decreasing absolute timing.
func TestCaptureTimingMonotonic(t *testing.T) {
	rec, _ := run("hello\r", "hello")
	var prev int64 = -1
	for _, e := range rec.Events {
		assert.That(t, e.TUS >= prev, "t_us is non-decreasing")
		prev = e.TUS
	}
}
