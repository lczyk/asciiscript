package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/lczyk/assert"
)

func TestBashCommand(t *testing.T) {
	cmd, cleanup, err := bashCommand(promptMarker{})
	assert.NoError(t, err)
	assert.ContainsString(t, cmd, "--rcfile")
	assert.ContainsString(t, cmd, "BASH_SILENCE_DEPRECATION_WARNING=1")

	fields := strings.Fields(cmd)
	path := fields[len(fields)-1]
	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "rcfile should exist before cleanup")

	cleanup()
	_, statErr = os.Stat(path)
	assert.That(t, os.IsNotExist(statErr), "rcfile should be gone after cleanup")
}

func TestStripQueries(t *testing.T) {
	in := []byte("before\x1b[6nmiddle\x1b[0c\x1b[?2026$p\x1b]11;?\x07after")
	assert.Equal(t, string(stripQueries(in)), "beforemiddleafter")
}

func TestStripQueriesNoQuery(t *testing.T) {
	assert.Equal(t, string(stripQueries([]byte("plain text\r\n"))), "plain text\r\n")
}

func TestEchoProbe(t *testing.T) {
	assert.Equal(t, echoProbe("echo hello world\n"), "echo hel")
	assert.Equal(t, echoProbe("ls\n"), "ls")
	assert.Equal(t, echoProbe("   \n"), "")
	assert.Equal(t, echoProbe("\n"), "")
}

func TestMirrorSaw(t *testing.T) {
	m, _ := newMarkedMirror(t)
	m.run(strings.NewReader("prompt$ echo hi\r\nhi\r\n"))
	assert.That(t, m.saw("echo hi"), "should have seen the echoed command")
	assert.That(t, !m.saw("echo bye"), "should not see what was never typed")
}

// A too-small --settle means asciinema swallows every keystroke; the run has to
// say so rather than produce an empty recording.
func TestConfirmEchoFailsWhenNothingComesBack(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	err := s.confirmEcho("echo hi\n", 0)
	assert.Error(t, err, "--settle")
}

func TestConfirmEchoPassesOnEcho(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	s.mon.head = []byte("prompt$ echo hi")
	assert.NoError(t, s.confirmEcho("echo hi\n", time.Second))
}

// `exit` only reaches bash if bash is the one reading the terminal; anything
// still holding it swallows the bytes, so the wait needs a deadline.
func TestFinishKillsWhatWontStop(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	assert.NoError(t, cmd.Start())

	err := finish(cmd, io.Discard, 50*time.Millisecond)
	assert.Error(t, err, "didn't stop within")
}

func TestFinishWaitsForACleanExit(t *testing.T) {
	cmd := exec.Command("true")
	assert.NoError(t, cmd.Start())

	assert.NoError(t, finish(cmd, io.Discard, 5*time.Second))
}

func TestPromptMarkerIsUniquePerRun(t *testing.T) {
	a, b := newPromptMarker(), newPromptMarker()

	assert.ContainsString(t, a.probe, "asciiscript=")
	assert.That(t, a.probe != b.probe, "each run should get its own marker")
}

// The marker rides in the prompt, so it has to sit inside \[ \] -- bash counts
// anything outside those towards the prompt's width and wraps the line early.
func TestPromptMarkerPrefixIsZeroWidth(t *testing.T) {
	m := newPromptMarker()

	prefix := m.prefix()
	assert.That(t, strings.HasPrefix(prefix, `\[`), `prefix should open with \[`)
	assert.That(t, strings.HasSuffix(prefix, `\]`), `prefix should close with \]`)
	assert.ContainsString(t, prefix, m.probe)
	assert.ContainsString(t, prefix, "$?") // exit status, for a later --fail-fast
}

// Continuation lines sit at PS2, so it needs the marker too -- otherwise every
// heredoc body line and every trailing backslash waits out the whole timeout.
func TestBashRCMarksBothPrompts(t *testing.T) {
	m := newPromptMarker()

	rc := bashRC(m)
	assert.ContainsString(t, rc, "PS1='"+m.prefix())
	assert.ContainsString(t, rc, "PS2='"+m.prefix())
}

// emitted is one prompt as the recorded shell writes it: the marker with its
// bash-level escapes resolved to real bytes.
func emitted(m promptMarker, status int) string {
	return fmt.Sprintf("\x1b]133;D;%d;%s\x07", status, m.probe)
}

func newMarkedMirror(t *testing.T) (*mirror, promptMarker) {
	t.Helper()
	m := newPromptMarker()
	return &mirror{quiet: true, mark: m}, m
}

func TestMirrorCountsPrompts(t *testing.T) {
	mon, m := newMarkedMirror(t)
	mon.run(strings.NewReader(emitted(m, 0) + "$ echo hi\r\nhi\r\n" + emitted(m, 0) + "$ "))
	assert.Equal(t, mon.marked(), 2)
}

// The pty splits reads wherever it happens to fill, so a marker routinely
// arrives in pieces. Missing those would hang every wait until the timeout.
func TestMirrorCountsAMarkerSplitAcrossReads(t *testing.T) {
	mon, m := newMarkedMirror(t)
	mon.run(iotest.OneByteReader(strings.NewReader(emitted(m, 0))))
	assert.Equal(t, mon.marked(), 1)
}

// The carried-over tail gets rescanned with the next read, so it must not let a
// marker count twice.
func TestMirrorCountsEachMarkerOnce(t *testing.T) {
	mon, m := newMarkedMirror(t)
	mon.run(iotest.OneByteReader(strings.NewReader(emitted(m, 0) + emitted(m, 1))))
	assert.Equal(t, mon.marked(), 2)
}

// A recording of asciiscript replayed inside asciiscript carries another run's
// markers; they must not read as prompts of this one.
func TestMirrorIgnoresAnotherRunsMarker(t *testing.T) {
	mon, _ := newMarkedMirror(t)
	theirs := newPromptMarker()

	mon.run(strings.NewReader(emitted(theirs, 0)))
	assert.Equal(t, mon.marked(), 0)
}

func TestMirrorCleanHidesTheMarker(t *testing.T) {
	mon, m := newMarkedMirror(t)
	out := mon.clean([]byte("before" + emitted(m, 127) + "\x1b[6nafter"))
	assert.Equal(t, string(out), "beforeafter")
}

func TestMirrorCleanLeavesOrdinaryOutput(t *testing.T) {
	mon, _ := newMarkedMirror(t)
	const line = "plain \x1b[1;34mtext\x1b[0m\r\n"
	assert.Equal(t, string(mon.clean([]byte(line))), line)
}

func TestSyncPromptReturnsOnANewPrompt(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	s.cmdTimeout = time.Minute
	s.mon.marks = 4

	assert.NoError(t, s.syncPrompt("sleep 1\n", 3))
	assert.Equal(t, warnings(s), "")
}

// The prompt already on screen when a line is typed is not that line finishing.
// Counting it would make every wait a no-op and the whole feature silent.
func TestSyncPromptWaitsPastThePromptItStartedFrom(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	s.cmdTimeout = 20 * time.Millisecond
	s.mon.marks = 3

	assert.NoError(t, s.syncPrompt("nano f\n", 3))
	assert.ContainsString(t, warnings(s), "hasn't finished")
}

// A command that holds the terminal never gives a prompt back. Typing on
// regardless is the old behaviour; the warning is what makes it diagnosable.
func TestSyncPromptWarnsAndCarriesOnAtTheTimeout(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	s.cmdTimeout = 20 * time.Millisecond

	assert.NoError(t, s.syncPrompt("nano rockcraft.yaml\n", 0))

	warning := warnings(s)
	assert.ContainsString(t, warning, "nano rockcraft.yaml")
	assert.ContainsString(t, warning, "#$ handover")
	assert.That(t, !strings.Contains(warning, `\n"`), "the trailing newline should be trimmed")
}

func TestSyncPromptStopsWhenInterrupted(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	s.cmdTimeout = time.Minute
	done := make(chan struct{})
	close(done)
	s.done = done
	s.sleep = s.realSleep

	assert.ErrorIs(t, s.syncPrompt("sleep 1\n", 0), errInterrupted)
}

// The whole point: the next line isn't typed until the previous one is done.
func TestTypeAllWaitsForEachCommand(t *testing.T) {
	s, rec := newRecordedScript(t, 0)
	s.commands = []command{newShell("a"), newShell("b")}
	s.wait = 0
	s.cmdTimeout = time.Minute
	s.mon.head = []byte("a") // stands in for the echo confirmEcho looks for

	// One prompt per line, and only once that line has been typed in full.
	rec.onWrite = func(p string) {
		if p == "\n" {
			s.mon.mu.Lock()
			s.mon.marks++
			s.mon.mu.Unlock()
		}
	}

	assert.NoError(t, s.typeAll(0))
	assert.EqualArrays(t, typedLines(rec), []string{"a\n", "b\n"})
	assert.Equal(t, warnings(s), "")
}

// With no prompt ever coming back, syncing must still let the script finish --
// slowly, a timeout per line, but finish.
func TestTypeAllTypesOnAfterTheTimeout(t *testing.T) {
	s, rec := newRecordedScript(t, 0)
	s.commands = []command{newShell("nano f"), newShell("b")}
	s.wait = 0
	s.cmdTimeout = time.Millisecond
	s.mon.head = []byte("nano f")

	assert.NoError(t, s.typeAll(0))
	assert.EqualArrays(t, typedLines(rec), []string{"nano f\n", "b\n"})
	assert.ContainsString(t, warnings(s), "nano f")
}

// `#$ delay` and `#$ wait` are only worth anything if they reach the typing, so
// check the gaps a script asks for are the gaps that come out.
func TestControlCommandsChangeTheTiming(t *testing.T) {
	s, err := parseScript("#$ delay 70\n#$ wait 250\nab\n")
	assert.NoError(t, err)

	rec := &recorder{}
	s.pty = rec
	s.sleep = rec.sleep
	s.jitter = newJitter(0, 1) // no jitter: every pause is exactly the delay
	s.warn = &bytes.Buffer{}
	s.mon.head = []byte("ab")

	assert.NoError(t, s.typeAll(0))
	assert.Equal(t, s.delay, 70*time.Millisecond)
	assert.Equal(t, s.wait, 250*time.Millisecond)
	assert.EqualArrays(t, rec.events, []string{
		"s:0s", // the settle
		"s:70ms", "w:a",
		"s:70ms", "w:b",
		"s:70ms", "w:\n",
		"s:250ms", // the wait, after the line
	})
}

// A pause belongs to the line it precedes, so `#$ wait N` slows down the line
// written under it -- not the one after that. Control lines cost nothing
// themselves: they are notes to asciiscript, never typed at the shell.
func TestWaitAppliesToTheLineThatFollowsIt(t *testing.T) {
	s, rec := newScriptFrom(t, "a\n#$ wait 500\nb\n")
	s.mon.head = []byte("a")

	assert.NoError(t, s.typeAll(0))
	assert.EqualArrays(t, rec.events, []string{
		"s:0s",                        // the settle
		"s:0s", "w:a", "s:0s", "w:\n", // first line: nothing to pause after yet
		"s:500ms",                     // the gap #$ wait asked for, before b
		"s:0s", "w:b", "s:0s", "w:\n", // and no pause charged for the control line
		"s:500ms", // a last beat before `exit` lands
	})
}

// The wait has to actually wait. Handing it a mark that is already there (as
// the cheaper tests do) can't tell a working wait from one that never runs, so
// this one makes the prompt arrive late, from another goroutine.
func TestSyncPromptPollsUntilThePromptArrives(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	s.cmdTimeout = 5 * time.Second
	s.sleep = s.realSleep // real sleeps: the polling is the thing under test

	const late = 60 * time.Millisecond
	go func() {
		time.Sleep(late)
		s.mon.mu.Lock()
		s.mon.marks = 1
		s.mon.mu.Unlock()
	}()

	start := time.Now()
	assert.NoError(t, s.syncPrompt("sleep 1\n", 0))
	took := time.Since(start)

	assert.That(t, took >= late, "should have waited for the prompt to come back")
	assert.That(t, took < time.Second, "should have carried on as soon as it did, not run out the timeout")
	assert.Equal(t, warnings(s), "")
}

// The same thing one level up, which is what a script actually relies on: the
// second line must not reach the pty until the first line's prompt is back.
func TestTypeAllHoldsTheNextLineUntilThePromptComesBack(t *testing.T) {
	s, rec := newRecordedScript(t, 0)
	s.commands = []command{newShell("first"), newShell("second")}
	s.wait = 0
	s.cmdTimeout = 5 * time.Second
	s.sleep = s.realSleep
	s.mon.head = []byte("first")

	// Each line's prompt comes back a while after it was typed, never during.
	const runtime = 50 * time.Millisecond
	rec.onWrite = func(p string) {
		if p != "\n" {
			return
		}
		go func() {
			time.Sleep(runtime)
			s.mon.mu.Lock()
			s.mon.marks++
			s.mon.mu.Unlock()
		}()
	}

	start := time.Now()
	assert.NoError(t, s.typeAll(0))

	assert.That(t, time.Since(start) >= 2*runtime, "both lines should have waited on their own prompt")
	assert.EqualArrays(t, typedLines(rec), []string{"first\n", "second\n"})
}
