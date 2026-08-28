package main

import (
	"bytes"
	"errors"
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

func TestParseScriptCommands(t *testing.T) {
	s, err := parseScript("echo hi\n#$ delay 100\n#$ wait 250\necho bye")
	assert.NoError(t, err)
	assert.Len(t, s.Commands, 4)

	sh, ok := s.Commands[0].(Shell)
	assert.That(t, ok, "command 0 should be a Shell")
	assert.Equal(t, sh.Cmd, "echo hi\n")

	d, ok := s.Commands[1].(Delay)
	assert.That(t, ok, "command 1 should be a Delay")
	assert.Equal(t, d.Interval, 100*time.Millisecond)

	w, ok := s.Commands[2].(Wait)
	assert.That(t, ok, "command 2 should be a Wait")
	assert.Equal(t, w.Duration, 250*time.Millisecond)

	sh2, ok := s.Commands[3].(Shell)
	assert.That(t, ok, "command 3 should be a Shell")
	assert.Equal(t, sh2.Cmd, "echo bye\n")
}

func TestParseScriptDefaults(t *testing.T) {
	s, err := parseScript("echo hi")
	assert.NoError(t, err)
	assert.Equal(t, s.Delay, 40*time.Millisecond)
	assert.Equal(t, s.Wait, 100*time.Millisecond)
}

func TestParseScriptSkipsBlankLines(t *testing.T) {
	s, err := parseScript("echo a\n\n\necho b\n")
	assert.NoError(t, err)
	assert.Len(t, s.Commands, 2)
}

func TestNewShellAppendsNewline(t *testing.T) {
	assert.Equal(t, NewShell("echo hi").Cmd, "echo hi\n")
	assert.Equal(t, NewShell("echo hi\n").Cmd, "echo hi\n")
}

func TestParseScriptUnknownCtrl(t *testing.T) {
	_, err := parseScript("#$ bogus 1")
	assert.ErrorIs(t, err, ErrUnknownCtrl)
}

func TestParseScriptCtrlNoArgs(t *testing.T) {
	_, err := parseScript("#$ delay")
	assert.ErrorIs(t, err, ErrNoArgs)
}

func TestParseScriptCtrlBadArg(t *testing.T) {
	_, err := parseScript("#$ wait abc")
	assert.ErrorIs(t, err, ErrBadArg)
}

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

// recorder stands in for the pty and the clock, so a Shell.Run can be replayed
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

func newRecordedScript(t *testing.T, delay time.Duration) (*Script, *recorder) {
	t.Helper()
	s, err := parseScript("")
	assert.NoError(t, err)
	rec := &recorder{}
	s.Delay = delay
	s.pty = rec
	s.sleep = rec.sleep
	s.jitter = newJitter(1, 7)
	s.warn = &bytes.Buffer{}
	return s, rec
}

// warnings is everything the script has reported to the user, the recorded
// session untouched.
func warnings(s *Script) string { return s.warn.(*bytes.Buffer).String() }

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

// Each pause belongs to the gap *before* its keystroke, so Shell.Run must wait
// it out and only then write. Typing first would shift every digraph and
// word-boundary pause one key late.
func TestShellRunWaitsBeforeEachKeystroke(t *testing.T) {
	s, rec := newRecordedScript(t, 40*time.Millisecond)
	plan := newJitter(1, 7).plan("ab\n", 40*time.Millisecond)

	assert.NoError(t, NewShell("ab").Run(s))

	assert.EqualArrays(t, rec.events, []string{
		"s:" + plan[0].pause.String(), "w:a",
		"s:" + plan[1].pause.String(), "w:b",
		"s:" + plan[2].pause.String(), "w:\n",
	})
}

func TestShellRunReturnsWriteError(t *testing.T) {
	s, rec := newRecordedScript(t, 0)
	rec.err = errors.New("pty is gone")

	err := NewShell("echo hi").Run(s)
	assert.Error(t, err, "writing to pty failed")
}

// An interrupted sleep aborts typing rather than finishing the line.
func TestShellRunStopsWhenInterrupted(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	done := make(chan struct{})
	close(done)
	s.done = done
	s.sleep = s.wait

	assert.ErrorIs(t, NewShell("echo hi").Run(s), ErrInterrupted)
}

func TestScriptWaitInterrupted(t *testing.T) {
	s, err := parseScript("")
	assert.NoError(t, err)
	done := make(chan struct{})
	close(done)
	s.done = done

	assert.ErrorIs(t, s.wait(time.Hour), ErrInterrupted)
	assert.ErrorIs(t, s.wait(0), ErrInterrupted)
	assert.NoError(t, (&Script{}).wait(0))
}

// Blank lines are script formatting between commands, but heredoc content is
// literal -- dropping a blank line there changes what the recorded shell reads.
func TestParseScriptKeepsHeredocBlankLines(t *testing.T) {
	s, err := parseScript("cat <<EOF > out.txt\nline one\n\nline three\nEOF\necho done\n")
	assert.NoError(t, err)

	var typed []string
	for _, c := range s.Commands {
		typed = append(typed, assert.Type[Shell](t, c).Cmd)
	}
	assert.EqualArrays(t, typed, []string{
		"cat <<EOF > out.txt\n", "line one\n", "\n", "line three\n", "EOF\n", "echo done\n",
	})
}

// Control lines inside a heredoc are content, not commands.
func TestParseScriptHeredocSwallowsCtrlLines(t *testing.T) {
	s, err := parseScript("cat <<EOF\n#$ delay 100\nEOF\n#$ delay 100\n")
	assert.NoError(t, err)
	assert.Len(t, s.Commands, 4)
	assert.Equal(t, assert.Type[Shell](t, s.Commands[1]).Cmd, "#$ delay 100\n")
	assert.Equal(t, assert.Type[Delay](t, s.Commands[3]).Interval, 100*time.Millisecond)
}

func TestHeredocDelim(t *testing.T) {
	for _, tc := range []struct {
		line  string
		delim string
		dash  bool
	}{
		{"cat <<EOF", "EOF", false},
		{"cat <<-EOF", "EOF", true},
		{"cat <<'EOF' > f", "EOF", false},
		{`cat <<"END"`, "END", false},
		{"cat << EOF", "EOF", false},
		{"cat <<<EOF", "", false}, // here-string
		{"echo a < b", "", false},
		{"echo 'a << b'", "", false},
		{`echo "a << b"`, "", false},
		{`echo "quoted" <<EOF`, "EOF", false},
	} {
		delim, dash := heredocDelim(tc.line)
		assert.Equal(t, delim, tc.delim, tc.line)
		assert.Equal(t, dash, tc.dash, tc.line)
	}
}

// <<- lets the terminator be indented with tabs.
func TestParseScriptHeredocDashTerminator(t *testing.T) {
	s, err := parseScript("cat <<-EOF\n\tbody\n\tEOF\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.Commands, 4)
	assert.Equal(t, assert.Type[Shell](t, s.Commands[3]).Cmd, "echo after\n")
}

func TestEchoProbe(t *testing.T) {
	assert.Equal(t, echoProbe("echo hello world\n"), "echo hel")
	assert.Equal(t, echoProbe("ls\n"), "ls")
	assert.Equal(t, echoProbe("   \n"), "")
	assert.Equal(t, echoProbe("\n"), "")
}

func TestMirrorSaw(t *testing.T) {
	m := &mirror{quiet: true}
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

// A `<<` inside quotes must not open a heredoc -- doing so would swallow the
// rest of the script as heredoc content.
func TestParseScriptIgnoresQuotedHeredocMarker(t *testing.T) {
	s, err := parseScript("echo \"write <<EOF to start one\"\n\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.Commands, 2)
	assert.Equal(t, assert.Type[Shell](t, s.Commands[1]).Cmd, "echo after\n")
}

func TestPromptMarkerIsUniquePerRun(t *testing.T) {
	a, err := newPromptMarker()
	assert.NoError(t, err)
	b, err := newPromptMarker()
	assert.NoError(t, err)

	assert.ContainsString(t, a.probe, "asciiscript=")
	assert.That(t, a.probe != b.probe, "each run should get its own marker")
}

// The marker rides in the prompt, so it has to sit inside \[ \] -- bash counts
// anything outside those towards the prompt's width and wraps the line early.
func TestPromptMarkerPrefixIsZeroWidth(t *testing.T) {
	m, err := newPromptMarker()
	assert.NoError(t, err)

	prefix := m.prefix()
	assert.That(t, strings.HasPrefix(prefix, `\[`), `prefix should open with \[`)
	assert.That(t, strings.HasSuffix(prefix, `\]`), `prefix should close with \]`)
	assert.ContainsString(t, prefix, m.probe)
	assert.ContainsString(t, prefix, "$?") // exit status, for a later --fail-fast
}

func TestPromptMarkerPrefixEmptyWhenUnset(t *testing.T) {
	assert.Equal(t, promptMarker{}.prefix(), "")
}

// Continuation lines sit at PS2, so it needs the marker too -- otherwise every
// heredoc body line and every trailing backslash waits out the whole timeout.
func TestBashRCMarksBothPrompts(t *testing.T) {
	m, err := newPromptMarker()
	assert.NoError(t, err)

	rc := bashRC(m)
	assert.ContainsString(t, rc, "PS1='"+m.prefix())
	assert.ContainsString(t, rc, "PS2='"+m.prefix())
}

func TestBashRCUnmarkedWithoutAMarker(t *testing.T) {
	rc := bashRC(promptMarker{})
	assert.That(t, !strings.Contains(rc, "133"), "no marker means no escape sequence")
	assert.ContainsString(t, rc, "PS1='")
}

// emitted is one prompt as the recorded shell writes it: the marker with its
// bash-level escapes resolved to real bytes.
func emitted(m promptMarker, status int) string {
	return fmt.Sprintf("\x1b]133;D;%d;%s\x07", status, m.probe)
}

func newMarkedMirror(t *testing.T) (*mirror, promptMarker) {
	t.Helper()
	m, err := newPromptMarker()
	assert.NoError(t, err)
	return &mirror{quiet: true, mark: m}, m
}

func TestMirrorCountsPrompts(t *testing.T) {
	mon, m := newMarkedMirror(t)
	mon.run(strings.NewReader(emitted(m, 0) + "$ echo hi\r\nhi\r\n" + emitted(m, 0) + "$ "))
	assert.Equal(t, mon.marked(), 2)
}

func TestMirrorCountsNothingWithoutAMarker(t *testing.T) {
	mon := &mirror{quiet: true}
	mon.run(strings.NewReader("\x1b]133;D;0;asciiscript=abc\x07$ "))
	assert.Equal(t, mon.marked(), 0)
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
	theirs, err := newPromptMarker()
	assert.NoError(t, err)

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
	s.syncFor = time.Minute
	s.mon.marks = 4

	assert.NoError(t, s.syncPrompt("sleep 1\n", 3))
	assert.Equal(t, warnings(s), "")
}

// The prompt already on screen when a line is typed is not that line finishing.
// Counting it would make every wait a no-op and the whole feature silent.
func TestSyncPromptWaitsPastThePromptItStartedFrom(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	s.syncFor = 20 * time.Millisecond
	s.mon.marks = 3

	assert.NoError(t, s.syncPrompt("nano f\n", 3))
	assert.ContainsString(t, warnings(s), "hasn't finished")
}

// A command that holds the terminal never gives a prompt back. Typing on
// regardless is the old behaviour; the warning is what makes it diagnosable.
func TestSyncPromptWarnsAndCarriesOnAtTheTimeout(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	s.syncFor = 20 * time.Millisecond

	assert.NoError(t, s.syncPrompt("nano rockcraft.yaml\n", 0))

	warning := warnings(s)
	assert.ContainsString(t, warning, "nano rockcraft.yaml")
	assert.ContainsString(t, warning, "--no-sync")
	assert.That(t, !strings.Contains(warning, `\n"`), "the trailing newline should be trimmed")
}

func TestSyncPromptStopsWhenInterrupted(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	s.syncFor = time.Minute
	done := make(chan struct{})
	close(done)
	s.done = done
	s.sleep = s.wait

	assert.ErrorIs(t, s.syncPrompt("sleep 1\n", 0), ErrInterrupted)
}

// The whole point: the next line isn't typed until the previous one is done.
func TestTypeAllWaitsForEachCommand(t *testing.T) {
	s, rec := newRecordedScript(t, 0)
	s.Commands = []Command{NewShell("a"), NewShell("b")}
	s.Wait = 0
	s.syncFor = time.Minute
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
	s.Commands = []Command{NewShell("nano f"), NewShell("b")}
	s.Wait = 0
	s.syncFor = time.Millisecond
	s.mon.head = []byte("nano f")

	assert.NoError(t, s.typeAll(0))
	assert.EqualArrays(t, typedLines(rec), []string{"nano f\n", "b\n"})
	assert.ContainsString(t, warnings(s), "nano f")
}

func TestTypeAllSkipsSyncingWhenOff(t *testing.T) {
	s, rec := newRecordedScript(t, 0)
	s.Commands = []Command{NewShell("a"), NewShell("b")}
	s.Wait = 0
	s.syncFor = 0
	s.mon.head = []byte("a")

	assert.NoError(t, s.typeAll(0))
	assert.EqualArrays(t, typedLines(rec), []string{"a\n", "b\n"})
	assert.Equal(t, warnings(s), "")
}
