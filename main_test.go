package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
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

// newScriptFrom is newRecordedScript for a script with actual content, typing
// at zero delay so the recorded events are the structure and nothing else.
func newScriptFrom(t *testing.T, text string) (*Script, *recorder) {
	t.Helper()
	s, err := parseScript(text)
	assert.NoError(t, err)
	rec := &recorder{}
	s.Delay = 0
	s.pty = rec
	s.sleep = rec.sleep
	s.jitter = newJitter(0, 1)
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

func TestParseScriptHandover(t *testing.T) {
	s, err := parseScript("#$ handover - over to you\nnano f\n")
	assert.NoError(t, err)
	assert.Len(t, s.Commands, 2)
	assert.Type[Handover](t, s.Commands[0])
	assert.Equal(t, assert.Type[Shell](t, s.Commands[1]).Cmd, "nano f\n")
}

func TestHandoverArmsTheNextLine(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	assert.That(t, !s.handover, "nothing armed to start with")

	assert.NoError(t, Handover{}.Run(s))
	assert.That(t, s.handover, "the next line should be armed")
	assert.ContainsString(t, warnings(s), "yours")
}

func TestScriptHasHandover(t *testing.T) {
	with, err := parseScript("#$ handover\nnano f\n")
	assert.NoError(t, err)
	assert.That(t, with.hasHandover(), "should spot the handover")

	without, err := parseScript("#$ wait 10\necho hi\n")
	assert.NoError(t, err)
	assert.That(t, !without.hasHandover(), "should not invent one")
}

// Each of these leaves the person driving either blind or stuck, so they're
// worth catching before a take starts rather than halfway through one.
func TestCheckHandoverRejectsUndriveableSettings(t *testing.T) {
	assert.Error(t, checkHandover(true, &Options{Quiet: true}), "--quiet")
	assert.Error(t, checkHandover(true, &Options{NoSync: true}), "--no-sync")

	// The suite's stdin isn't a terminal, which is the third trap.
	assert.Error(t, checkHandover(true, &Options{}), "isn't a terminal")
}

func TestCheckHandoverIgnoresScriptsWithout(t *testing.T) {
	assert.NoError(t, checkHandover(false, &Options{Quiet: true, NoSync: true}))
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

	assert.NoError(t, s.handOver(0))
	assert.That(t, restored, "the terminal should be handed back")
}

func TestHandOverFailsWhenTheTerminalWont(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	s.raw = func() (func(), error) { return nil, errors.New("not a terminal") }

	assert.Error(t, s.handOver(0), "not a terminal")
}

func TestHandOverStopsWhenInterrupted(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	s.raw = func() (func(), error) { return func() {}, nil }
	done := make(chan struct{})
	close(done)
	s.done = done
	s.sleep = s.wait

	assert.ErrorIs(t, s.handOver(0), ErrInterrupted)
}

// The armed line waits on the keyboard rather than the clock, and only that
// line: the one after it goes back to the ordinary timed sync.
func TestTypeAllHandsOverOnlyTheArmedLine(t *testing.T) {
	s, rec := newRecordedScript(t, 0)
	s.Commands = []Command{Handover{}, NewShell("nano f"), NewShell("echo after")}
	s.Wait = 0
	s.syncFor = time.Minute
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
	assert.That(t, !s.handover, "the arming should be spent")
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

func TestTermSizeUsesWhatWasAskedFor(t *testing.T) {
	cols, rows := termSize(&Options{Cols: 100, Rows: 30})
	assert.Equal(t, cols, uint16(100))
	assert.Equal(t, rows, uint16(30))
}

// A dimension left at zero gets filled in from somewhere -- the real terminal,
// or 80x24 when there isn't one -- without disturbing the one that was given.
func TestTermSizeFillsInWhatWasNot(t *testing.T) {
	cols, rows := termSize(&Options{Cols: 100})
	assert.Equal(t, cols, uint16(100))
	assert.That(t, rows > 0, "rows should be filled in")

	cols, rows = termSize(&Options{Rows: 30})
	assert.Equal(t, rows, uint16(30))
	assert.That(t, cols > 0, "cols should be filled in")

	cols, rows = termSize(&Options{})
	assert.That(t, cols > 0 && rows > 0, "both should be filled in")
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
	assert.Equal(t, s.Delay, 70*time.Millisecond)
	assert.Equal(t, s.Wait, 250*time.Millisecond)
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
	s.syncFor = 5 * time.Second
	s.sleep = s.wait // real sleeps: the polling is the thing under test

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
	s.Commands = []Command{NewShell("first"), NewShell("second")}
	s.Wait = 0
	s.syncFor = 5 * time.Second
	s.sleep = s.wait
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
