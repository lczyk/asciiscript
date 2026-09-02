//go:build !windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/lczyk/assert"
)

// The rcfile's path travels in the environment, not the command: asciinema
// runs the command through `sh -c`, and a path with a space in it would come
// apart there.
func TestBashCommand(t *testing.T) {
	cmd, env, cleanup, err := bashCommand(promptMarker{})
	assert.NoError(t, err)
	assert.ContainsString(t, cmd, `--rcfile "$`+rcfileVar+`"`)
	assert.That(t, slices.Contains(env, "BASH_SILENCE_DEPRECATION_WARNING=1"), "env should silence the macOS banner")

	var path string
	for _, kv := range env {
		if p, ok := strings.CutPrefix(kv, rcfileVar+"="); ok {
			path = p
		}
	}
	assert.That(t, path != "", "env should carry the rcfile path")
	assert.That(t, !strings.Contains(cmd, path), "the command should not name the temp file")
	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "rcfile should exist before cleanup")

	cleanup()
	_, statErr = os.Stat(path)
	assert.That(t, os.IsNotExist(statErr), "rcfile should be gone after cleanup")
}

// finish has to report a pty that is already gone, and still reap asciinema.
func TestFinishReportsAFailedEnd(t *testing.T) {
	cmd := exec.Command("true")
	assert.NoError(t, cmd.Start())

	err := finish(cmd, &recorder{err: errors.New("pty is gone")}, 5*time.Second)
	assert.Error(t, err, "couldn't end the session")
	assert.Error(t, err, "pty is gone")
	assert.That(t, cmd.ProcessState != nil, "asciinema should still have been waited for")
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

func TestMirrorSawAfter(t *testing.T) {
	m, _ := newMarkedMirror(t)
	m.run(strings.NewReader("prompt$ echo hi\r\nhi\r\n"))
	assert.That(t, m.sawAfter("echo hi", 0), "should have seen the echoed command")
	assert.That(t, !m.sawAfter("echo bye", 0), "should not see what was never typed")

	// Only output from the point on counts.
	assert.That(t, !m.sawAfter("echo hi", m.seen()), "should not see what came before")
	assert.That(t, !m.sawAfter("echo hi", m.seen()+100), "a point past the end is the end")
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

// The marker is the seed's, so a take pinned with --seed carries the same one
// and two runs with different seeds don't mistake each other's prompts.
func TestPromptMarkerFollowsTheSeed(t *testing.T) {
	a, b, c := newPromptMarker(1), newPromptMarker(2), newPromptMarker(1)

	assert.ContainsString(t, a.probe, "asciiscript=")
	assert.That(t, a.probe != b.probe, "each seed should get its own marker")
	assert.Equal(t, a.probe, c.probe, "the same seed should get the same marker")
}

// The marker rides in the prompt, so it has to sit inside \[ \] -- bash counts
// anything outside those towards the prompt's width and wraps the line early.
func TestPromptMarkerPrefixIsZeroWidth(t *testing.T) {
	m := newPromptMarker(7)

	prefix := m.prefix()
	assert.That(t, strings.HasPrefix(prefix, `\[`), `prefix should open with \[`)
	assert.That(t, strings.HasSuffix(prefix, `\]`), `prefix should close with \]`)
	assert.ContainsString(t, prefix, m.probe)
	assert.ContainsString(t, prefix, "$?") // the exit status rides along, as OSC 133 has it
}

// Continuation lines sit at PS2, so it needs the marker too -- otherwise every
// heredoc body line and every trailing backslash waits out the whole timeout.
// Both are read-only, so a script can't take the marker away.
func TestBashRCMarksBothPrompts(t *testing.T) {
	m := newPromptMarker(7)

	rc := bashRC(m)
	assert.ContainsString(t, rc, "PS1='"+m.prefix())
	assert.ContainsString(t, rc, "PS2='"+m.prefix())
	assert.That(t, strings.HasSuffix(rc, "readonly PS1 PS2\n"), "the prompts should be read-only")
}

// The prompt's working directory is fish's prompt_pwd: ~ for home, parents cut
// to a letter (keeping a dotdir's dot), the last component whole. Run through
// the real bash on PATH, since the function has to hold up on 3.2 as well as 5.
func TestBashRCAbbreviatesTheWorkingDirectory(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash isn't on PATH")
	}
	home := t.TempDir()
	rc := filepath.Join(home, "rc")
	assert.NoError(t, os.WriteFile(rc, []byte(bashRC(promptMarker{})), 0o600))

	for _, tc := range []struct{ dir, want string }{
		{home, "~"},
		{filepath.Join(home, "git", "asciiscript"), "~/g/asciiscript"},
		{filepath.Join(home, ".config", "fish", "functions"), "~/.c/f/functions"},
		{"/", "/"},
		{"/usr/local/bin", "/u/l/bin"},
		{filepath.Join(home, "has space", "dir"), "~/h/dir"},
		{filepath.Join(home, "a-directory-with-a-long-name"), "~/a-directory-with-a-long-name"},
	} {
		assert.NoError(t, os.MkdirAll(tc.dir, 0o755))
		cmd := exec.Command("bash", "--noprofile", "--norc", "-c", `source "$1" && cd "$2" && __asciiscript_pwd`, "_", rc, tc.dir)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		assert.NoError(t, err, tc.dir)
		assert.Equal(t, string(out), tc.want, tc.dir)
		assert.Equal(t, abbrevPwd(tc.dir, home), tc.want, tc.dir) // the Go twin, for the width estimate
	}
}

// A line that will wrap is called out with the width it wraps at; the rest
// are counted. What counts as wrapping depends on the prompt in front.
func TestWarnWideLines(t *testing.T) {
	sc := &script{commands: []command{
		cmd("ls"),
		cmd(strings.Repeat("x", 70)),
		{lines: []string{"cat <<EOF", strings.Repeat("y", 79), "EOF"}},
	}}

	var w bytes.Buffer
	warnWideLines(sc, 80, "~/g/asciiscript$ ", &w)
	assert.ContainsString(t, w.String(), strings.Repeat("x", 70))
	assert.ContainsString(t, w.String(), "80 columns")
	assert.ContainsString(t, w.String(), "(and 1 more)")

	w.Reset()
	warnWideLines(sc, 80, "$ ", &w)
	assert.ContainsString(t, w.String(), strings.Repeat("y", 79))
	assert.That(t, !strings.Contains(w.String(), "more"), "only one line is too wide behind a short prompt")

	w.Reset()
	warnWideLines(sc, 100, "~/g/asciiscript$ ", &w)
	assert.Equal(t, w.String(), "")
}

// emitted is one prompt as the recorded shell writes it: the marker with its
// bash-level escapes resolved to real bytes.
func emitted(m promptMarker, status int) string {
	return fmt.Sprintf("\x1b]133;D;%d;%s\x07", status, m.probe)
}

func newMarkedMirror(t *testing.T) (*mirror, promptMarker) {
	t.Helper()
	m := newPromptMarker(7)
	return &mirror{quiet: true, mark: m}, m
}

// The probe text on its own is not a prompt: `echo "$PS1"` prints it, and
// counting that would type the next line into whatever runs after.
func TestMirrorIgnoresTheBareProbe(t *testing.T) {
	mon, m := newMarkedMirror(t)
	mon.run(strings.NewReader("$ echo \"$PS1\"\r\n" + m.prefix() + "$ \r\n" + m.probe + "\r\n"))
	assert.Equal(t, mon.marked(), 0)
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
	theirs := newPromptMarker(8)

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

func TestConfirmEchoFailsWhenNothingComesBack(t *testing.T) {
	s, _ := newTestSession(t)
	s.echoGrace = time.Millisecond
	err := s.confirmEcho("echo hi", 0)
	assert.Error(t, err, "never echoed back")
}

func TestConfirmEchoPassesOnEcho(t *testing.T) {
	s, _ := newTestSession(t)
	s.mon.head = []byte("$ echo hi\r\nhi\r\n")
	assert.NoError(t, s.confirmEcho("echo hi", 0))
}

// The shell's own startup output -- its prompt shows the working directory --
// can carry the probe by chance, so only what came after the line was typed
// counts as its echo.
func TestConfirmEchoIgnoresWhatCameBefore(t *testing.T) {
	s, _ := newTestSession(t)
	s.echoGrace = time.Millisecond
	s.mon.head = []byte("~/tools$ ")
	assert.Error(t, s.confirmEcho("ls", s.mon.seen()), "never echoed back")
}

// Each pause belongs to the gap *before* its keystroke, so typeLine must wait
// it out and only then write. Typing first would shift every digraph and
// word-boundary pause one key late.
func TestTypeLineWaitsBeforeEachKeystroke(t *testing.T) {
	s, rec := newTestSession(t)
	plan := newJitter(1, 7).plan("ab\n", 40*time.Millisecond, 0)

	assert.NoError(t, s.typeLine("ab", 40*time.Millisecond, 0))

	assert.EqualArrays(t, rec.events, []string{
		"s:" + plan[0].pause.String(), "w:a",
		"s:" + plan[1].pause.String(), "w:b",
		"s:" + plan[2].pause.String(), "w:\n",
	})
}

// --speed scales everything the script says about timing, the pause included:
// twice as fast is the whole take twice as fast.
func TestTypeLineScalesWithSpeed(t *testing.T) {
	s, rec := newTestSession(t)
	s.jitter = newJitter(0, 1)
	s.speed = 2

	assert.NoError(t, s.typeLine("ab", 40*time.Millisecond, 500*time.Millisecond))
	assert.EqualArrays(t, rec.events, []string{
		"s:250ms", "w:a", "s:20ms", "w:b", "s:20ms", "w:\n",
	})
}

func TestTypeLineReturnsWriteError(t *testing.T) {
	s, rec := newTestSession(t)
	rec.err = errors.New("pty is gone")

	err := s.typeLine("echo hi", 0, 0)
	assert.Error(t, err, "writing to pty failed")
}

// An interrupted sleep aborts typing rather than finishing the line.
func TestTypeLineStopsWhenInterrupted(t *testing.T) {
	s, _ := newTestSession(t)
	done := make(chan struct{})
	close(done)
	s.done = done
	s.sleep = s.realSleep

	assert.ErrorIs(t, s.typeLine("echo hi", 0, 0), errInterrupted)
}

func TestRealSleepInterrupted(t *testing.T) {
	s := newSession()
	done := make(chan struct{})
	close(done)
	s.done = done

	assert.ErrorIs(t, s.realSleep(time.Hour), errInterrupted)
	assert.ErrorIs(t, s.realSleep(0), errInterrupted)
	assert.NoError(t, (&session{}).realSleep(0))
}

func TestSyncPromptReturnsOnANewPrompt(t *testing.T) {
	s, _ := newTestSession(t)
	s.cmdTimeout = time.Minute
	s.mon.marks = 4

	assert.NoError(t, s.syncPrompt("sleep 1", 3))
	assert.Equal(t, warnings(s), "")
}

// The prompt already on screen when a line is typed is not that line finishing.
// Counting it would make every wait a no-op and the whole feature silent.
func TestSyncPromptWaitsPastThePromptItStartedFrom(t *testing.T) {
	s, _ := newTestSession(t)
	s.cmdTimeout = 20 * time.Millisecond
	s.mon.marks = 3

	assert.NoError(t, s.syncPrompt("nano f", 3))
	assert.ContainsString(t, warnings(s), "hasn't finished")
}

// A command that holds the terminal never gives a prompt back. Typing on
// regardless is deliberate; the warning is what makes it diagnosable.
func TestSyncPromptWarnsAndCarriesOnAtTheTimeout(t *testing.T) {
	s, _ := newTestSession(t)
	s.cmdTimeout = 20 * time.Millisecond

	assert.NoError(t, s.syncPrompt("nano rockcraft.yaml", s.mon.marked()))

	warning := warnings(s)
	assert.ContainsString(t, warning, "nano rockcraft.yaml")
	assert.ContainsString(t, warning, "#$ handover")
}

func TestSyncPromptStopsWhenInterrupted(t *testing.T) {
	s, _ := newTestSession(t)
	s.cmdTimeout = time.Minute
	done := make(chan struct{})
	close(done)
	s.done = done
	s.sleep = s.realSleep

	assert.ErrorIs(t, s.syncPrompt("sleep 1", s.mon.marked()), errInterrupted)
}

// promptsOnEnter makes the recorder stand in for the shell: each line is
// echoed as it's typed, and a prompt follows once it has been typed in full.
func promptsOnEnter(s *session, rec *recorder) {
	rec.onWrite = func(p string) {
		s.mon.mu.Lock()
		s.mon.head = append(s.mon.head, p...)
		if p == "\n" {
			s.mon.marks++
		}
		s.mon.mu.Unlock()
	}
}

// echoesOnEnter is a shell that echoes what's typed but never prompts again.
func echoesOnEnter(s *session, rec *recorder) {
	rec.onWrite = func(p string) {
		s.mon.mu.Lock()
		s.mon.head = append(s.mon.head, p...)
		s.mon.mu.Unlock()
	}
}

// The whole point: the next line isn't typed until the previous one is done.
func TestTypeAllWaitsForEachCommand(t *testing.T) {
	s, rec := newTestSession(t)
	s.cmdTimeout = time.Minute
	promptsOnEnter(s, rec)

	assert.NoError(t, s.typeAll(&script{commands: []command{cmd("a"), cmd("b")}}))
	assert.EqualArrays(t, typedLines(rec), []string{"a\n", "b\n"})
	assert.Equal(t, warnings(s), "")
}

// Nothing is typed until the shell has shown a prompt: that is when asciinema
// is up and forwarding input, and no fixed wait can know when that is.
func TestTypeAllWaitsForTheFirstPrompt(t *testing.T) {
	s, rec := newTestSession(t)
	s.cmdTimeout = time.Minute
	s.sleep = s.realSleep
	s.mon.marks = 0
	promptsOnEnter(s, rec)

	const late = 60 * time.Millisecond
	go func() {
		time.Sleep(late)
		s.mon.mu.Lock()
		s.mon.marks = 1
		s.mon.mu.Unlock()
	}()

	start := time.Now()
	assert.NoError(t, s.typeAll(&script{commands: []command{cmd("a")}}))
	assert.That(t, time.Since(start) >= late, "should have waited for the first prompt")
	assert.EqualArrays(t, typedLines(rec), []string{"a\n"})
}

// With no prompt ever coming back, syncing must still let the script finish --
// slowly, a timeout per line, but finish.
func TestTypeAllTypesOnAfterTheTimeout(t *testing.T) {
	s, rec := newTestSession(t)
	s.cmdTimeout = time.Millisecond
	echoesOnEnter(s, rec)

	assert.NoError(t, s.typeAll(&script{commands: []command{cmd("nano f"), cmd("b")}}))
	assert.EqualArrays(t, typedLines(rec), []string{"nano f\n", "b\n"})
	assert.ContainsString(t, warnings(s), "nano f")
}

// `#$ delay` and `#$ pause` are only worth anything if they reach the typing,
// so check the gaps a script asks for are the gaps that come out -- and that
// they are the one command's, with the next back on the defaults.
func TestTypeAllTimesEachCommandAsAsked(t *testing.T) {
	sc, err := parseScript("#$ delay 70\n#$ pause 250\nab\nc\n#$ pause 900\n")
	assert.NoError(t, err)

	s, rec := newTestSession(t)
	s.jitter = newJitter(0, 1) // no jitter: every pause is exactly what was asked
	promptsOnEnter(s, rec)

	assert.NoError(t, s.typeAll(sc))
	assert.EqualArrays(t, rec.events, []string{
		"s:250ms", "w:a", "s:70ms", "w:b", "s:70ms", "w:\n",
		"s:40ms", "w:c", "s:40ms", "w:\n", // back on the defaults, the model's own line gap in front
		"s:900ms", // the trailing pause, holding the last prompt
	})
}

// The pause the script asks for is for the command, so its first line. The
// lines a heredoc runs on to get the model's ordinary line gap.
func TestTypeAllPausesBeforeTheFirstLineOnly(t *testing.T) {
	sc, err := parseScript("#$ pause 500\ncat <<EOF\nbody\nEOF\n")
	assert.NoError(t, err)

	s, rec := newTestSession(t)
	s.jitter = newJitter(0, 1)
	promptsOnEnter(s, rec)

	assert.NoError(t, s.typeAll(sc))
	assert.Equal(t, rec.events[0], "s:500ms")
	assert.EqualArrays(t, typedLines(rec), []string{"cat <<EOF\n", "body\n", "EOF\n"})
	var paused int
	for _, e := range rec.events {
		if e == "s:500ms" {
			paused++
		}
	}
	assert.Equal(t, paused, 1, "body and EOF should get the model's own line gap, not the pause again")
}

// The wait has to actually wait. Handing it a mark that is already there (as
// the cheaper tests do) can't tell a working wait from one that never runs, so
// this one makes the prompt arrive late, from another goroutine.
func TestSyncPromptPollsUntilThePromptArrives(t *testing.T) {
	s, _ := newTestSession(t)
	s.cmdTimeout = 5 * time.Second
	s.sleep = s.realSleep // real sleeps: the polling is the thing under test

	const late = 60 * time.Millisecond
	go func() {
		time.Sleep(late)
		s.mon.mu.Lock()
		s.mon.marks++
		s.mon.mu.Unlock()
	}()

	start := time.Now()
	assert.NoError(t, s.syncPrompt("sleep 1", s.mon.marked()))
	took := time.Since(start)

	assert.That(t, took >= late, "should have waited for the prompt to come back")
	assert.That(t, took < time.Second, "should have carried on as soon as it did, not run out the timeout")
	assert.Equal(t, warnings(s), "")
}

// The same thing one level up, which is what a script actually relies on: the
// second line must not reach the pty until the first line's prompt is back.
func TestTypeAllHoldsTheNextLineUntilThePromptComesBack(t *testing.T) {
	s, rec := newTestSession(t)
	s.cmdTimeout = 5 * time.Second
	s.sleep = s.realSleep

	// Each line's prompt comes back a while after it was typed, never during.
	const runtime = 50 * time.Millisecond
	rec.onWrite = func(p string) {
		s.mon.mu.Lock()
		s.mon.head = append(s.mon.head, p...)
		s.mon.mu.Unlock()
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
	assert.NoError(t, s.typeAll(&script{commands: []command{cmd("first"), cmd("second")}}))

	assert.That(t, time.Since(start) >= 2*runtime, "both lines should have waited on their own prompt")
	assert.EqualArrays(t, typedLines(rec), []string{"first\n", "second\n"})
}
