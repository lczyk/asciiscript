package main

import (
	"errors"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

func TestParseScriptCommands(t *testing.T) {
	s, err := parseScript("echo hi\n#$ delay 100\n#$ wait 250\necho bye")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 4)

	sh, ok := s.commands[0].(shell)
	assert.That(t, ok, "command 0 should be a shell")
	assert.Equal(t, sh.cmd, "echo hi\n")

	d, ok := s.commands[1].(setDelay)
	assert.That(t, ok, "command 1 should be a setDelay")
	assert.Equal(t, d.d, 100*time.Millisecond)

	w, ok := s.commands[2].(setWait)
	assert.That(t, ok, "command 2 should be a setWait")
	assert.Equal(t, w.d, 250*time.Millisecond)

	sh2, ok := s.commands[3].(shell)
	assert.That(t, ok, "command 3 should be a shell")
	assert.Equal(t, sh2.cmd, "echo bye\n")
}

func TestParseScriptDefaults(t *testing.T) {
	s, err := parseScript("echo hi")
	assert.NoError(t, err)
	assert.Equal(t, s.delay, 40*time.Millisecond)
	assert.Equal(t, s.wait, 100*time.Millisecond)
}

func TestParseScriptSkipsBlankLines(t *testing.T) {
	s, err := parseScript("echo a\n\n\necho b\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
}

func TestNewShellAppendsNewline(t *testing.T) {
	assert.Equal(t, newShell("echo hi").cmd, "echo hi\n")
	assert.Equal(t, newShell("echo hi\n").cmd, "echo hi\n")
}

// A control line is words, however they are spaced: a second space or a tab
// before the number is not a missing argument.
func TestParseScriptCtrlLooseSpacing(t *testing.T) {
	s, err := parseScript("#$  delay \t 100  \n#$wait 5")
	assert.NoError(t, err)
	assert.Equal(t, assert.Type[setDelay](t, s.commands[0]).d, 100*time.Millisecond)
	assert.Equal(t, assert.Type[setWait](t, s.commands[1]).d, 5*time.Millisecond)
}

func TestParseScriptUnknownCtrl(t *testing.T) {
	_, err := parseScript("#$ bogus 1")
	assert.ErrorIs(t, err, errUnknownCtrl)
}

func TestParseScriptCtrlNoArgs(t *testing.T) {
	_, err := parseScript("#$ delay")
	assert.ErrorIs(t, err, errNoArgs)
}

func TestParseScriptCtrlBadArg(t *testing.T) {
	_, err := parseScript("#$ wait abc")
	assert.ErrorIs(t, err, errBadArg)
}

// Each pause belongs to the gap *before* its keystroke, so shell.Run must wait
// it out and only then write. Typing first would shift every digraph and
// word-boundary pause one key late.
func TestShellRunWaitsBeforeEachKeystroke(t *testing.T) {
	s, rec := newRecordedScript(t, 40*time.Millisecond)
	plan := newJitter(1, 7).plan("ab\n", 40*time.Millisecond)

	assert.NoError(t, newShell("ab").run(s))

	assert.EqualArrays(t, rec.events, []string{
		"s:" + plan[0].pause.String(), "w:a",
		"s:" + plan[1].pause.String(), "w:b",
		"s:" + plan[2].pause.String(), "w:\n",
	})
}

func TestShellRunReturnsWriteError(t *testing.T) {
	s, rec := newRecordedScript(t, 0)
	rec.err = errors.New("pty is gone")

	err := newShell("echo hi").run(s)
	assert.Error(t, err, "writing to pty failed")
}

// An interrupted sleep aborts typing rather than finishing the line.
func TestShellRunStopsWhenInterrupted(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	done := make(chan struct{})
	close(done)
	s.done = done
	s.sleep = s.realSleep

	assert.ErrorIs(t, newShell("echo hi").run(s), errInterrupted)
}

func TestScriptWaitInterrupted(t *testing.T) {
	s, err := parseScript("")
	assert.NoError(t, err)
	done := make(chan struct{})
	close(done)
	s.done = done

	assert.ErrorIs(t, s.realSleep(time.Hour), errInterrupted)
	assert.ErrorIs(t, s.realSleep(0), errInterrupted)
	assert.NoError(t, (&script{}).realSleep(0))
}

// Blank lines are script formatting between commands, but heredoc content is
// literal -- dropping a blank line there changes what the recorded shell reads.
func TestParseScriptKeepsHeredocBlankLines(t *testing.T) {
	s, err := parseScript("cat <<EOF > out.txt\nline one\n\nline three\nEOF\necho done\n")
	assert.NoError(t, err)

	var typed []string
	for _, c := range s.commands {
		typed = append(typed, assert.Type[shell](t, c).cmd)
	}
	assert.EqualArrays(t, typed, []string{
		"cat <<EOF > out.txt\n", "line one\n", "\n", "line three\n", "EOF\n", "echo done\n",
	})
}

// Control lines inside a heredoc are content, not commands.
func TestParseScriptHeredocSwallowsCtrlLines(t *testing.T) {
	s, err := parseScript("cat <<EOF\n#$ delay 100\nEOF\n#$ delay 100\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 4)
	assert.Equal(t, assert.Type[shell](t, s.commands[1]).cmd, "#$ delay 100\n")
	assert.Equal(t, assert.Type[setDelay](t, s.commands[3]).d, 100*time.Millisecond)
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
	assert.Len(t, s.commands, 4)
	assert.Equal(t, assert.Type[shell](t, s.commands[3]).cmd, "echo after\n")
}

// A `<<` inside quotes must not open a heredoc -- doing so would swallow the
// rest of the script as heredoc content.
func TestParseScriptIgnoresQuotedHeredocMarker(t *testing.T) {
	s, err := parseScript("echo \"write <<EOF to start one\"\n\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.Equal(t, assert.Type[shell](t, s.commands[1]).cmd, "echo after\n")
}

func TestParseScriptHandover(t *testing.T) {
	s, err := parseScript("#$ handover - over to you\nnano f\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.Type[handover](t, s.commands[0])
	assert.Equal(t, assert.Type[shell](t, s.commands[1]).cmd, "nano f\n")
}

func TestHandoverArmsTheNextLine(t *testing.T) {
	s, _ := newRecordedScript(t, 0)
	assert.That(t, !s.armed, "nothing armed to start with")

	assert.NoError(t, handover{}.run(s))
	assert.That(t, s.armed, "the next line should be armed")
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
