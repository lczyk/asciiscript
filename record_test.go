//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/lczyk/assert"
)

// A seed is four digits: read off the screen, typed back in as --seed.
func TestRandomSeedIsFourDigits(t *testing.T) {
	for range 1000 {
		seed := randomSeed()
		assert.That(t, seed >= 0 && seed < 10000, "seed %d should have at most four digits", seed)
	}
}

func TestWriteRC(t *testing.T) {
	m := newPromptMarker(7)
	path, cleanup, err := writeRC(m)
	assert.NoError(t, err)
	b, readErr := os.ReadFile(path)
	assert.NoError(t, readErr, "rcfile should exist before cleanup")
	assert.Equal(t, string(b), bashRC(m))

	cleanup()
	_, statErr := os.Stat(path)
	assert.That(t, os.IsNotExist(statErr), "rcfile should be gone after cleanup")
}

// drained stands in for the mirror having read the pty to its end.
func drained() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// finish has to report a pty that is already gone, and still reap the shell.
func TestFinishReportsAFailedEnd(t *testing.T) {
	cmd := exec.Command("true")
	assert.NoError(t, cmd.Start())

	status, err := finish(cmd, &recorder{err: errors.New("pty is gone")}, drained(), 5*time.Second)
	assert.Error(t, err, "couldn't end the session")
	assert.Error(t, err, "pty is gone")
	assert.That(t, cmd.ProcessState != nil, "the shell should still have been waited for")
	assert.Equal(t, status, 0)
}

// How the shell ended is what the recording's last event says: the exit code,
// or 128 plus the signal, as $? would have it.
func TestFinishReportsTheExitStatus(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 7")
	assert.NoError(t, cmd.Start())
	status, err := finish(cmd, io.Discard, drained(), 5*time.Second)
	assert.NoError(t, err)
	assert.Equal(t, status, 7)

	cmd = exec.Command("sleep", "30")
	assert.NoError(t, cmd.Start())
	status, err = finish(cmd, io.Discard, drained(), 50*time.Millisecond)
	assert.Error(t, err, "didn't exit within")
	assert.Equal(t, status, 137, "a kill is 128 + SIGKILL")
}

// The last of the shell's output can arrive after it has exited; finish waits
// for the mirror to read it, but only for so long.
func TestFinishWaitsForTheDrainBriefly(t *testing.T) {
	cmd := exec.Command("true")
	assert.NoError(t, cmd.Start())
	never := make(chan struct{})

	start := time.Now()
	_, err := finish(cmd, io.Discard, never, 5*time.Second)
	assert.NoError(t, err)
	took := time.Since(start)
	assert.That(t, took >= drainGrace, "should have given the drain its grace")
	assert.That(t, took < 5*time.Second, "but not waited for the exit timeout")
}

func TestStripQueries(t *testing.T) {
	in := []byte("before\x1b[6nmiddle\x1b[0c\x1b[?2026$p\x1b]11;?\x07after")
	assert.Equal(t, string(stripQueries(in)), "beforemiddleafter")
}

func TestStripQueriesNoQuery(t *testing.T) {
	assert.Equal(t, string(stripQueries([]byte("plain text\r\n"))), "plain text\r\n")
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
	}
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

// Each pause belongs to the gap *before* its keystroke, so typeLine must wait
// it out and only then write. Typing first would shift every digraph and
// word-boundary pause one key late.
func TestTypeLineWaitsBeforeEachKeystroke(t *testing.T) {
	s, rec := newTestSession(t)
	plan := newJitter(1, 7).plan("ab\n", 40*time.Millisecond, 0)

	assert.NoError(t, s.typeLine("ab", 40*time.Millisecond, 0, nil))

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

	assert.NoError(t, s.typeLine("ab", 40*time.Millisecond, 500*time.Millisecond, nil))
	assert.EqualArrays(t, rec.events, []string{
		"s:250ms", "w:a", "s:20ms", "w:b", "s:20ms", "w:\n",
	})
}

func TestTypeLineReturnsWriteError(t *testing.T) {
	s, rec := newTestSession(t)
	rec.err = errors.New("pty is gone")

	err := s.typeLine("echo hi", 0, 0, nil)
	assert.Error(t, err, "writing to pty failed")
}

// An interrupted sleep aborts typing rather than finishing the line.
func TestTypeLineStopsWhenInterrupted(t *testing.T) {
	s, _ := newTestSession(t)
	done := make(chan struct{})
	close(done)
	s.done = done
	s.sleep = s.realSleep

	assert.ErrorIs(t, s.typeLine("echo hi", 0, 0, nil), errInterrupted)
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

// promptsOnEnter makes the recorder stand in for the shell: one prompt per
// line, and only once that line has been typed in full.
func promptsOnEnter(s *session, rec *recorder) {
	rec.onWrite = func(p string) {
		if p == "\n" {
			s.mon.mu.Lock()
			s.mon.marks++
			s.mon.mu.Unlock()
		}
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

// Nothing is typed until the shell has shown a prompt: that is when the rcfile
// has loaded and readline is listening, and no fixed wait can know when that is.
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

// castInto is a recording written to a file under t's temp dir with a clock
// that never moves, so the events' order is all that varies.
func castInto(t *testing.T, s *session) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "take.cast")
	f, err := os.Create(path)
	assert.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	at := time.Unix(0, 0)
	cast, err := newCastWriter(f, castHeader{Term: castTerm{Cols: 80, Rows: 24}}, func() time.Time { return at })
	assert.NoError(t, err)
	s.cast, s.mon.cast = cast, cast
	t.Cleanup(func() { _ = cast.close() })
	return path
}

// A `#$ pause` puts a marker in the recording where the typing resumes, named
// after the command, so a player can jump from one paused command to the next.
// It lands after the pause and before the first keystroke -- with the typed
// keys recorded as well, that is between the previous line's newline and the
// paused command's first letter.
func TestTypeAllMarksAPausedCommand(t *testing.T) {
	s, rec := newTestSession(t)
	s.cmdTimeout = time.Minute
	s.captureInput = true
	promptsOnEnter(s, rec)
	path := castInto(t, s)

	sc, err := parseScript("echo a\n#$ pause 500\necho b\necho c\n")
	assert.NoError(t, err)
	assert.NoError(t, s.typeAll(sc))
	assert.NoError(t, s.cast.exit(0))
	assert.NoError(t, s.cast.close())

	events := readCast(t, path)
	var markers []int
	for i, e := range events {
		if e.kind == "m" {
			markers = append(markers, i)
		}
	}
	assert.Len(t, markers, 1, "one paused command, one marker")
	m := markers[0]
	assert.Equal(t, events[m].data, "echo b")
	assert.Equal(t, events[m-1].kind+events[m-1].data, "i\n", "the marker follows the previous command's newline")
	assert.Equal(t, events[m+1].kind+events[m+1].data, "ie", "and precedes the paused command's first key")
}

// Keystrokes go into the recording only when asked for.
func TestTypeAllRecordsInputOnlyWhenAsked(t *testing.T) {
	for _, capture := range []bool{false, true} {
		s, rec := newTestSession(t)
		s.cmdTimeout = time.Minute
		s.captureInput = capture
		promptsOnEnter(s, rec)
		path := castInto(t, s)

		assert.NoError(t, s.typeAll(&script{commands: []command{cmd("echo hi")}}))
		assert.NoError(t, s.cast.exit(0))
		assert.NoError(t, s.cast.close())

		var typed strings.Builder
		for _, e := range readCast(t, path) {
			if e.kind == "i" {
				typed.WriteString(e.data)
			}
		}
		if capture {
			assert.Equal(t, typed.String(), "echo hi\n")
		} else {
			assert.Equal(t, typed.String(), "")
		}
	}
}

// A recording that can't be written any more is not worth typing the rest of
// the script into.
func TestTypeAllStopsWhenTheRecordingFails(t *testing.T) {
	s, rec := newTestSession(t)
	s.cmdTimeout = time.Minute
	promptsOnEnter(s, rec)
	castInto(t, s)
	s.cast.err = errors.New("disk full")

	err := s.typeAll(&script{commands: []command{cmd("echo a"), cmd("echo b")}})
	assert.Error(t, err, "disk full")
	assert.Len(t, typedLines(rec), 0)
}

// The mirror tees what it reads into the recording before anything is
// stripped from the live echo: the recording is the session as it was.
func TestMirrorWritesTheRecording(t *testing.T) {
	s, _ := newTestSession(t)
	s.mon.mark = newPromptMarker(7)
	s.mon.quiet = true
	path := castInto(t, s)

	s.mon.run(strings.NewReader(emitted(s.mon.mark, 0) + "$ \x1b[6nhi\r\n"))
	assert.NoError(t, s.cast.close())

	got := output(readCast(t, path))
	assert.Equal(t, got, emitted(s.mon.mark, 0)+"$ \x1b[6nhi\r\n")
}
