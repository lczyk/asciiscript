package main

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

// Each of these leaves the person driving either blind or stuck, so they're
// worth catching before a take starts rather than halfway through one.
func TestCheckHandoverRejectsUndriveableSettings(t *testing.T) {
	assert.Error(t, checkHandover(true, &options{Quiet: true}), "--quiet")

	// The suite's stdin isn't a terminal, which is the other trap.
	assert.Error(t, checkHandover(true, &options{}), "isn't a terminal")
}

func TestCheckHandoverIgnoresScriptsWithout(t *testing.T) {
	assert.NoError(t, checkHandover(false, &options{Quiet: true}))
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
	s, _ := newRecordedScript(t, 0)
	s.mon.marks = 1

	restored := false
	s.raw = func() (func(), error) { return func() { restored = true }, nil }

	assert.NoError(t, s.lendTerminal(0))
	assert.That(t, restored, "the terminal should be handed back")
}

func TestHandOverFailsWhenTheTerminalWont(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	s.raw = func() (func(), error) { return nil, errors.New("not a terminal") }

	assert.Error(t, s.lendTerminal(0), "not a terminal")
}

func TestHandOverStopsWhenInterrupted(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	s.raw = func() (func(), error) { return func() {}, nil }
	done := make(chan struct{})
	close(done)
	s.done = done
	s.sleep = s.realSleep

	assert.ErrorIs(t, s.lendTerminal(0), errInterrupted)
}

// The armed line waits on the keyboard rather than the clock, and only that
// line: the one after it goes back to the ordinary timed sync.
func TestTypeAllHandsOverOnlyTheArmedLine(t *testing.T) {
	s, rec := newRecordedScript(t, 0)
	s.commands = []command{handover{}, newShell("nano f"), newShell("echo after")}
	s.wait = 0
	s.cmdTimeout = time.Minute
	s.mon.head = []byte("nano f")

	lent := 0
	s.raw = func() (func(), error) { lent++; return func() {}, nil }

	// The shell only comes back to a prompt once the whole line has been typed.
	rec.onWrite = func(p string) {
		if p == "\n" {
			s.mon.mu.Lock()
			s.mon.marks++
			s.mon.mu.Unlock()
		}
	}

	assert.NoError(t, s.typeAll(0))
	assert.EqualArrays(t, typedLines(rec), []string{"nano f\n", "echo after\n"})
	assert.Equal(t, lent, 1)
	assert.That(t, !s.armed, "the arming should be spent")
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
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if s.String() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("keyboard input never arrived: want %q, got %q", want, s.String())
}
