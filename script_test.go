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

// A negative delay would type instantly; one past the ceiling would hang the
// recording, and a large enough one would overflow the Duration it becomes.
func TestParseScriptCtrlArgOutOfRange(t *testing.T) {
	for _, text := range []string{
		"a\n#$ delay -5\nb",
		"a\n#$ pause 9223372036854775807\nb",
		"a\n#$ delay 3600001\nb",
	} {
		_, err := parseScript(text)
		assert.ErrorIs(t, err, errArgRange, text)
		assert.Error(t, err, "line 2", text)
	}
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
		{"cat <<END-OF-FILE", "END-OF-FILE", false},
		{`cat <<\EOF`, "EOF", false}, // backslash-quoted, same as 'EOF'
		{`cat <<-\EOF`, "EOF", true},
		{"cat <<A;echo b", "A", false}, // the word stops at the metacharacter
		{"cat <<A|cat", "A", false},
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

// A heredoc delimiter is a bash word: it can hold characters ParseInt-style
// identifier rules would reject, and a backslash in front of it quotes it the
// same as wrapping it in quotes would.
func TestParseScriptHeredocWordDelimiter(t *testing.T) {
	s, err := parseScript("cat <<END-OF-FILE\nbody\nEND-OF-FILE\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[0].lines, []string{"cat <<END-OF-FILE", "body", "END-OF-FILE"})

	s, err = parseScript("cat <<\\EOF\nbody\nEOF\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[0].lines, []string{"cat <<\\EOF", "body", "EOF"})
}

// `cat <<A <<B` opens two heredocs: bash reads body A, terminator A, body B,
// terminator B, in that order, before the command is done.
func TestParseScriptMultipleHeredocsOnOneLine(t *testing.T) {
	s, err := parseScript("cat <<A <<-B\nbody a\nA\n\tbody b\n\tB\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[0].lines, []string{
		"cat <<A <<-B", "body a", "A", "\tbody b", "\tB",
	})
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

// Inside a `$'...'` string a backslash escapes the next character, so an
// escaped quote does not close the string.
func TestParseScriptAnsiCQuote(t *testing.T) {
	s, err := parseScript("echo $'don\\'t stop'\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[0].lines, []string{"echo $'don\\'t stop'"})

	_, err = parseScript("echo $'open\n")
	assert.ErrorIs(t, err, errUnterminated)
}

// `#` starts a comment not just after whitespace but wherever it starts a
// word, which includes right after a metacharacter that ended the last one.
func TestParseScriptCommentAfterMetachar(t *testing.T) {
	for _, text := range []string{
		"echo a;#comment\necho b\n",
		"echo hi|#comment\necho b\n",
		"echo a;#comment \\\necho b\n", // the trailing backslash is inside the comment
	} {
		s, err := parseScript(text)
		assert.NoError(t, err, text)
		assert.Len(t, s.commands, 2, text)
	}

	s, err := parseScript("echo a#b\necho c\n")
	assert.NoError(t, err)
	assert.EqualArrays(t, s.commands[0].lines, []string{"echo a#b"})
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
		ansiIn    bool // q == '\'' is a `$'...'` quote continued from a previous line
		quote     byte
		ansi      bool
		backslash bool
		literal   string // the line with every literal byte replaced by '_'
	}{
		{"echo hi", 0, false, 0, false, false, "echo hi"},
		{"echo 'a b' c", 0, false, 0, false, false, "echo _____ c"},
		{`echo "a b" c`, 0, false, 0, false, false, "echo _____ c"},
		{`echo "a`, 0, false, '"', false, false, "echo __"},
		{"b\" c", '"', false, 0, false, false, "__ c"},
		{"echo 'a", 0, false, '\'', false, false, "echo __"},
		{`echo "a\"b"`, 0, false, 0, false, false, "echo ______"},
		{`echo \"a`, 0, false, 0, false, false, "echo \\\"a"},
		{"echo a \\", 0, false, 0, false, true, "echo a \\"},
		{"echo a \\\\", 0, false, 0, false, false, "echo a \\\\"},
		{"echo 'a \\", 0, false, '\'', false, false, "echo ____"},
		{"echo \"a \\", 0, false, '"', false, true, "echo ____"},
		{"echo a # b \\", 0, false, 0, false, false, "echo a _____"},
		{"# whole line \\", 0, false, 0, false, false, "______________"},
		{"echo a#b \\", 0, false, 0, false, true, "echo a#b \\"},
		{"echo a;#comment", 0, false, 0, false, false, "echo a;________"},
		{"echo hi|#comment", 0, false, 0, false, false, "echo hi|________"},
		{"echo a;#comment \\", 0, false, 0, false, false, "echo a;__________"},
		// `$'don\'t stop'`: the escaped quote stays inside the ANSI-C string,
		// which the trailing unescaped one closes.
		{`$'a\'b'`, 0, false, 0, false, false, "$______"},
		// the escaped `$` opens a plain quote, so the backslash inside it is
		// just another literal byte, not an escape.
		{`\$'a\'`, 0, false, 0, false, false, "\\$____"},
		// an ANSI-C quote still open from a previous line honours the escape.
		{"more\\'text'", '\'', true, 0, false, false, "___________"},
	} {
		sc := scanLine(tc.line, tc.q, tc.ansiIn)
		assert.Equal(t, sc.quote, tc.quote, tc.line)
		assert.Equal(t, sc.ansi, tc.ansi, tc.line)
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

// A heredoc delimiter is one bash word however it is quoted: quoted runs
// and bare runs concatenate, and a backslash escapes the byte after it.
func TestParseScriptHeredocMixedQuotingInDelimiter(t *testing.T) {
	for _, tc := range []struct{ text, delim string }{
		{"cat <<\"EOF\"x\nbody\nEOFx\necho after\n", "EOFx"},
		{"cat <<EO\"F\"\nbody\nEOF\necho after\n", "EOF"},
		{"cat <<E\\ OF\nbody\nE OF\necho after\n", "E OF"},
	} {
		s, err := parseScript(tc.text)
		assert.NoError(t, err, tc.text)
		assert.Len(t, s.commands, 2, tc.text)
		assert.EqualArrays(t, s.commands[0].lines, []string{strings.SplitN(tc.text, "\n", 2)[0], "body", tc.delim}, tc.text)
		assert.EqualArrays(t, s.commands[1].lines, []string{"echo after"}, tc.text)
	}
}

// The command line a heredoc is opened on can itself run on -- a trailing
// backslash, an open quote -- and bash reads that continuation before the
// heredoc's body.
func TestParseScriptHeredocAfterTheCommandLineEnds(t *testing.T) {
	s, err := parseScript("cat <<EOF \\\n  > out.txt\nbody\nEOF\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[0].lines, []string{"cat <<EOF \\", "  > out.txt", "body", "EOF"})

	s, err = parseScript("cat <<EOF \"a\nb\"\nbody\nEOF\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[0].lines, []string{"cat <<EOF \"a", "b\"", "body", "EOF"})
}

// `$$` is the shell's pid, not a `$` in front of a quote: the quote after it
// is a plain one, in which a backslash is just a backslash.
func TestParseScriptDoubleDollarBeforeAQuote(t *testing.T) {
	s, err := parseScript("echo $$'a\\'b'\nc'\necho after\n")
	assert.NoError(t, err)
	assert.Len(t, s.commands, 2)
	assert.EqualArrays(t, s.commands[0].lines, []string{"echo $$'a\\'b'", "c'"})
}
