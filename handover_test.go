//go:build !windows

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

// Each of these leaves the person driving either blind or stuck, so they're
// worth catching before a take starts rather than halfway through one.
func TestCheckHandoverRejectsUndriveableSettings(t *testing.T) {
	assert.Error(t, checkHandover(true, &options{Quiet: true}, true), "--quiet")
	assert.Error(t, checkHandover(true, &options{}, false), "isn't a terminal")
	assert.NoError(t, checkHandover(true, &options{}, true))
}

func TestCheckHandoverIgnoresScriptsWithout(t *testing.T) {
	assert.NoError(t, checkHandover(false, &options{Quiet: true}, false))
}

// A keypress only belongs in the recording while the terminal is on loan.
func TestKeyboardOnlyForwardsWhileLent(t *testing.T) {
	in, feed := io.Pipe()
	defer feed.Close()
	k := &keyboard{in: in}

	var got bytes.Buffer
	sink := &syncWriter{w: &got}
	back := k.lend(sink)
	_, err := feed.Write([]byte("^O"))
	assert.NoError(t, err)
	sink.await(t, "^O")

	back()
	_, err = feed.Write([]byte("dropped"))
	assert.NoError(t, err)

	// Nothing more can arrive, but give the pump a chance to get it wrong.
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, sink.String(), "^O")
}

// A second handover has to get the keyboard back, not find it still parked on
// the first one's pty.
func TestKeyboardCanBeLentAgain(t *testing.T) {
	in, feed := io.Pipe()
	defer feed.Close()
	k := &keyboard{in: in}

	first := &syncWriter{w: &bytes.Buffer{}}
	back := k.lend(first)
	back()

	second := &syncWriter{w: &bytes.Buffer{}}
	defer k.lend(second)()
	_, err := feed.Write([]byte("y"))
	assert.NoError(t, err)
	second.await(t, "y")
	assert.Equal(t, first.String(), "")
}

// The handover ends when the shell offers a prompt again -- when whoever was
// driving quit the editor -- and not on any clock.
func TestHandOverEndsOnThePrompt(t *testing.T) {
	s, _ := newTestSession(t)

	restored := false
	s.raw = func() (func(), error) { return func() { restored = true }, nil }

	assert.NoError(t, s.lendTerminal(0))
	assert.That(t, restored, "the terminal should be handed back")
	assert.That(t, !s.mon.lent.Load(), "queries should be stripped again once the terminal is back")
}

// While the terminal is on loan the recorded session's queries go through to
// it, since the person driving is there to have them answered.
func TestHandOverLetsQueriesThrough(t *testing.T) {
	s, _ := newTestSession(t)
	s.raw = func() (func(), error) { return func() {}, nil }
	s.mon.marks = 0

	// The wait polls through sleep, which is the one moment mid-handover a
	// test can look in on.
	var lent bool
	s.sleep = func(time.Duration) error {
		lent = s.mon.lent.Load()
		s.mon.marks = 1
		return nil
	}

	assert.NoError(t, s.lendTerminal(0))
	assert.That(t, lent, "queries should pass through during the handover")
	assert.That(t, !s.mon.lent.Load(), "and be stripped again after it")
}

func TestHandOverFailsWhenTheTerminalWont(t *testing.T) {
	s, _ := newTestSession(t)
	s.raw = func() (func(), error) { return nil, errors.New("not a terminal") }

	assert.Error(t, s.lendTerminal(0), "not a terminal")
}

func TestHandOverStopsWhenInterrupted(t *testing.T) {
	s, _ := newTestSession(t)
	s.raw = func() (func(), error) { return func() {}, nil }
	done := make(chan struct{})
	close(done)
	s.done = done
	s.sleep = s.realSleep

	assert.ErrorIs(t, s.lendTerminal(s.mon.marked()), errInterrupted)
}

// The handed-over command waits on the keyboard rather than the clock, and
// only that one: the command after it goes back to the ordinary timed sync.
func TestTypeAllHandsOverOnlyTheMarkedCommand(t *testing.T) {
	s, rec := newTestSession(t)
	s.cmdTimeout = time.Minute
	promptsOnEnter(s, rec)

	lent := 0
	s.raw = func() (func(), error) { lent++; return func() {}, nil }

	nano := cmd("nano f")
	nano.handover = true
	assert.NoError(t, s.typeAll(&script{commands: []command{nano, cmd("echo after")}}))
	assert.EqualArrays(t, typedLines(rec), []string{"nano f\n", "echo after\n"})
	assert.Equal(t, lent, 1)
	assert.ContainsString(t, warnings(s), "yours")
}

// A multi-line command is handed over once all of it is typed; its earlier
// lines sit at PS2 and are waited on like any other.
func TestTypeAllHandsOverAfterTheLastLine(t *testing.T) {
	s, rec := newTestSession(t)
	s.cmdTimeout = time.Minute
	promptsOnEnter(s, rec)

	lentAfter := -1
	s.raw = func() (func(), error) { lentAfter = len(rec.events); return func() {}, nil }

	assert.NoError(t, s.typeAll(&script{commands: []command{
		{lines: []string{"python3 \\", "  -q"}, handover: true},
	}}))
	assert.Equal(t, lentAfter, len(rec.events), "should be lent only once everything is typed")
}

// syncWriter is a bytes.Buffer safe to read from one goroutine while the
// keyboard pump writes to it from another.
type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.String()
}

// await blocks until want has arrived, so a test doesn't race the pump.
func (s *syncWriter) await(t *testing.T, want string) {
	t.Helper()
	assert.Eventually(t, func() bool { return s.String() == want }, 2*time.Second, time.Millisecond,
		"keyboard input never arrived: want %q, got %q", want, s.String())
}

// A resize while the terminal is on loan reaches the recording as a resize
// event: lendTerminal watches for SIGWINCH and calls the session's resize,
// which is where the pty is sized and the event written.
func TestHandOverRecordsAResize(t *testing.T) {
	s, _ := newTestSession(t)
	s.raw = func() (func(), error) { return func() {}, nil }
	s.mon.marks = 0
	path := castInto(t, s)

	var resized atomic.Bool
	s.resize = func() {
		_ = s.cast.resize(100, 40)
		resized.Store(true)
	}
	sent := false
	s.sleep = func(time.Duration) error {
		if !sent {
			sent = true
			assert.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGWINCH))
		}
		if resized.Load() {
			s.mon.marks = 1
		}
		time.Sleep(time.Millisecond)
		return nil
	}

	assert.NoError(t, s.lendTerminal(0))
	assert.NoError(t, s.cast.exit(0))
	assert.NoError(t, s.cast.close())

	var sizes []string
	for _, e := range readCast(t, path) {
		if e.kind == "r" {
			sizes = append(sizes, e.data)
		}
	}
	assert.EqualArrays(t, sizes, []string{"100x40"})
}
