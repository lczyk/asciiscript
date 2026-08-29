package main

import (
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

// typed flattens a script back to the lines it would type, in order.
func typed(s *script) []string {
	var lines []string
	for _, c := range s.commands {
		lines = append(lines, c.lines...)
	}
	return lines
}

func TestParseScriptCommands(t *testing.T) {
	s, err := parseScript("echo hi\n#$ delay 100\n#$ pause 250\necho bye")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[0].lines, []string{"echo hi"})
	assert.EqualArrays(t, s.commands[1].lines, []string{"echo bye"})
	assert.Equal(t, s.commands[1].delay, 100*time.Millisecond)
	assert.Equal(t, s.commands[1].pause, 250*time.Millisecond)
}

func TestParseScriptDefaults(t *testing.T) {
	s, err := parseScript("echo hi")
	assert.NoError(t, err)
	assert.Equal(t, s.commands[0].delay, defaultDelay)
	assert.Equal(t, s.commands[0].pause, time.Duration(0))
	assert.That(t, !s.commands[0].handover, "nothing to hand over")
	assert.Equal(t, s.pause, time.Duration(0))
}

// A control line is for the one command after it. The command after that is
// back on the defaults, whatever the script set earlier.
func TestParseScriptCtrlIsNotSticky(t *testing.T) {
	s, err := parseScript("#$ delay 100\n#$ pause 250\n#$ handover\na\nb\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)

	a, b := s.commands[0], s.commands[1]
	assert.Equal(t, a.delay, 100*time.Millisecond)
	assert.Equal(t, a.pause, 250*time.Millisecond)
	assert.That(t, a.handover, "a should be handed over")
	assert.Equal(t, b.delay, defaultDelay)
	assert.Equal(t, b.pause, time.Duration(0))
	assert.That(t, !b.handover, "b should not be")
}

// Blank lines between commands are formatting, whitespace-only ones included,
// and a control line still finds its command across them.
func TestParseScriptSkipsBlankLines(t *testing.T) {
	s, err := parseScript("echo a\n\n  \t \n#$ delay 10\n\necho b\n")
	assert.NoError(t, err)
	assert.EqualArrays(t, typed(s), []string{"echo a", "echo b"})
	assert.Equal(t, s.commands[1].delay, 10*time.Millisecond)
}

// A control line is words, however they are spaced or indented: a second space
// or a tab before the number is not a missing argument.
func TestParseScriptCtrlLooseSpacing(t *testing.T) {
	for _, text := range []string{
		"#$ delay 100\na",
		"#$   delay 100\na",
		"#$ delay     100\na",
		"#$\tdelay\t100\t\na",
		"#$delay 100\na",
		"   #$ delay 100\na",
	} {
		s, err := parseScript(text)
		assert.NoError(t, err, text)
		assert.Equal(t, s.commands[0].delay, 100*time.Millisecond, text)
	}
}

func TestParseScriptUnknownCtrl(t *testing.T) {
	_, err := parseScript("#$ bogus 1\na")
	assert.ErrorIs(t, err, errUnknownCtrl)
	assert.Error(t, err, "line 1")
}

func TestParseScriptCtrlNoArgs(t *testing.T) {
	_, err := parseScript("#$ delay\na")
	assert.ErrorIs(t, err, errNoArgs)
}

func TestParseScriptCtrlBadArg(t *testing.T) {
	_, err := parseScript("a\n#$ pause abc\nb")
	assert.ErrorIs(t, err, errBadArg)
	assert.Error(t, err, "line 2")
}

// A pause at the end of the script holds the last prompt before the session
// is ended. The other two have nothing to apply to, which is a mistake worth
// hearing about rather than a line silently ignored.
func TestParseScriptTrailingCtrl(t *testing.T) {
	s, err := parseScript("echo hi\n#$ pause 1000\n")
	assert.NoError(t, err)
	assert.Equal(t, s.pause, time.Second)
	assert.Len(t, s.commands, 1)

	_, err = parseScript("echo hi\n#$ delay 10\n")
	assert.ErrorIs(t, err, errDangling)
	assert.Error(t, err, "line 2")

	_, err = parseScript("echo hi\n\n#$ handover\n\n")
	assert.ErrorIs(t, err, errDangling)
	assert.Error(t, err, "line 3")

	_, err = parseScript("#$ pause 5\n#$ delay 10\n")
	assert.ErrorIs(t, err, errDangling)
}

// Blank lines are script formatting between commands, but heredoc content is
// literal -- dropping a blank line there changes what the recorded shell reads.
func TestParseScriptHeredocIsOneCommand(t *testing.T) {
	s, err := parseScript("#$ delay 10\ncat <<EOF > out.txt\nline one\n\nline three\nEOF\necho done\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[0].lines, []string{
		"cat <<EOF > out.txt", "line one", "", "line three", "EOF",
	})
	assert.Equal(t, s.commands[0].delay, 10*time.Millisecond)
	assert.EqualArrays(t, s.commands[1].lines, []string{"echo done"})
}

// Control lines inside a heredoc are content, not commands.
func TestParseScriptHeredocSwallowsCtrlLines(t *testing.T) {
	s, err := parseScript("cat <<EOF\n#$ delay 100\nEOF\n#$ delay 100\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[0].lines, []string{"cat <<EOF", "#$ delay 100", "EOF"})
	assert.Equal(t, s.commands[0].delay, defaultDelay)
	assert.Equal(t, s.commands[1].delay, 100*time.Millisecond)
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
		{"echo hi # cat <<EOF", "", false}, // a comment
		{`echo \"a <<EOF`, "EOF", false},   // the escaped quote opens nothing
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
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[1].lines, []string{"echo after"})
}

// A `<<` inside quotes must not open a heredoc -- doing so would swallow the
// rest of the script as heredoc content.
func TestParseScriptIgnoresQuotedHeredocMarker(t *testing.T) {
	s, err := parseScript("echo \"write <<EOF to start one\"\n\necho after\n")
	assert.NoError(t, err)
	assert.EqualArrays(t, typed(s), []string{"echo \"write <<EOF to start one\"", "echo after"})
}

// A line ending in a backslash continues on the next one, and bash reads
// everything there literally -- a "#$" line is typed, and a blank line is how
// the continuation ends.
func TestParseScriptBackslashContinuation(t *testing.T) {
	s, err := parseScript("#$ delay 10\necho one \\\n  two \\\n#$ three\n\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[0].lines, []string{"echo one \\", "  two \\", "#$ three"})
	assert.Equal(t, s.commands[0].delay, 10*time.Millisecond)
	assert.EqualArrays(t, s.commands[1].lines, []string{"echo after"})

	s, err = parseScript("echo one \\\n\necho after\n")
	assert.NoError(t, err)
	assert.EqualArrays(t, s.commands[0].lines, []string{"echo one \\", ""})
	assert.EqualArrays(t, s.commands[1].lines, []string{"echo after"})
}

// An escaped backslash at the end of a line is a backslash, not a continuation;
// one inside single quotes is text; one in a comment is nothing.
func TestParseScriptBackslashThatDoesNotContinue(t *testing.T) {
	for _, text := range []string{
		"echo a \\\\\necho b\n",
		"echo 'a\\'\necho b\n",
		"echo a # \\\necho b\n",
	} {
		s, err := parseScript(text)
		assert.NoError(t, err, text)
		assert.Len(t, s.commands, 2, text)
	}
}

// A quote left open runs the command on to the line that closes it.
func TestParseScriptOpenQuoteContinuation(t *testing.T) {
	s, err := parseScript("echo \"one\n\n#$ two\nthree\" && echo four\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[0].lines, []string{"echo \"one", "", "#$ two", "three\" && echo four"})

	s, err = parseScript("echo 'it''s\nfine' # \"\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
}

// A heredoc opened on a line that a quote continued onto is still a heredoc.
func TestParseScriptHeredocAfterContinuation(t *testing.T) {
	s, err := parseScript("echo \"a\nb\" && cat <<EOF\n\nEOF\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[0].lines, []string{"echo \"a", "b\" && cat <<EOF", "", "EOF"})
}

// A command that never ends -- heredoc without its terminator, quote never
// closed, backslash on the last line -- would hang the recording at PS2.
func TestParseScriptUnterminated(t *testing.T) {
	for _, text := range []string{
		"cat <<EOF\nbody\n",
		"echo \"open\n",
		"echo one \\\n",
		"echo one \\",
	} {
		_, err := parseScript(text)
		assert.ErrorIs(t, err, errUnterminated, text)
	}
}

func TestScanLine(t *testing.T) {
	for _, tc := range []struct {
		line      string
		q         byte
		quote     byte
		backslash bool
		literal   string // the line with every literal byte replaced by '_'
	}{
		{"echo hi", 0, 0, false, "echo hi"},
		{"echo 'a b' c", 0, 0, false, "echo _____ c"},
		{`echo "a b" c`, 0, 0, false, "echo _____ c"},
		{`echo "a`, 0, '"', false, "echo __"},
		{"b\" c", '"', 0, false, "__ c"},
		{"echo 'a", 0, '\'', false, "echo __"},
		{`echo "a\"b"`, 0, 0, false, "echo ______"},
		{`echo \"a`, 0, 0, false, "echo \\\"a"},
		{"echo a \\", 0, 0, true, "echo a \\"},
		{"echo a \\\\", 0, 0, false, "echo a \\\\"},
		{"echo 'a \\", 0, '\'', false, "echo ____"},
		{"echo \"a \\", 0, '"', true, "echo ____"},
		{"echo a # b \\", 0, 0, false, "echo a _____"},
		{"# whole line \\", 0, 0, false, "______________"},
		{"echo a#b \\", 0, 0, true, "echo a#b \\"},
	} {
		sc := scanLine(tc.line, tc.q)
		assert.Equal(t, sc.quote, tc.quote, tc.line)
		assert.Equal(t, sc.backslash, tc.backslash, tc.line)
		var got strings.Builder
		for i, lit := range sc.literal {
			if lit {
				got.WriteByte('_')
			} else {
				got.WriteByte(tc.line[i])
			}
		}
		assert.Equal(t, got.String(), tc.literal, tc.line)
	}
}

func TestParseScriptHandover(t *testing.T) {
	s, err := parseScript("#$ handover - over to you\nnano f\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 1)
	assert.That(t, s.commands[0].handover, "should be handed over")
	assert.EqualArrays(t, s.commands[0].lines, []string{"nano f"})
}

func TestScriptHasHandover(t *testing.T) {
	with, err := parseScript("echo hi\n#$ handover\nnano f\n")
	assert.NoError(t, err)
	assert.That(t, with.hasHandover(), "should spot the handover")

	without, err := parseScript("#$ pause 10\necho hi\n")
	assert.NoError(t, err)
	assert.That(t, !without.hasHandover(), "should not invent one")
}
