package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ctrlPrefix marks a control line in a script, e.g. "#$ delay 100".
const ctrlPrefix = "#$"

// defaultDelay is the per-keystroke delay a command types at unless a
// `#$ delay` in front of it says otherwise.
const defaultDelay = 40 * time.Millisecond

// maxDelay is the longest a `#$ delay` or `#$ pause` may ask for.
const maxDelay = time.Hour

var (
	errUnknownCtrl  = errors.New("unknown control command")
	errNoArgs       = errors.New("no arguments given to command")
	errBadArg       = errors.New("invalid command argument")
	errArgRange     = errors.New("argument out of range")
	errDangling     = errors.New("control line with no command after it to apply to")
	errUnterminated = errors.New("command runs past the end of the script")
	errInterrupted  = errors.New("interrupted")
)

// command is one shell command -- the lines it spans, and how to type it.
// Control lines set the timing of the one command that follows them, and only
// that one; a command with none in front of it gets the defaults.
type command struct {
	lines    []string      // physical lines, typed one after another
	delay    time.Duration // between keystrokes
	pause    time.Duration // before the first keystroke; zero for the timing model's own line gap
	handover bool          // hand the terminal over once the last line is typed
}

// script is a parsed script: the commands to type in order, and how long to
// hold the last prompt before the session is ended.
type script struct {
	commands []command
	pause    time.Duration // a trailing `#$ pause`
}

// hasHandover reports whether the script hands the terminal over at any point.
func (s *script) hasHandover() bool {
	return slices.ContainsFunc(s.commands, func(c command) bool { return c.handover })
}

// loadScript parses a script from the file at path.
func loadScript(path string) (*script, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseScript(string(b))
}

// parseScript parses a script from raw script text.
//
// A command normally is one line. It runs on while a heredoc is open, while a
// quote is open, or after a line ending in a backslash -- and inside one every
// line is literal, blank lines and "#$" lines included, because bash will read
// them as part of the command. Between commands, blank lines are skipped and
// control lines are collected for the next command to be typed.
func parseScript(text string) (*script, error) {
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // the newline the file ends with, not a blank line
	}

	s := &script{}
	next := command{delay: defaultDelay} // what the control lines so far have asked of the next command
	delaySet, ctrlAt := false, 0         // ctrlAt is the last control line's number; 0 when none is pending
	var cont continuation

	for i, raw := range lines {
		if cont.open() {
			last := &s.commands[len(s.commands)-1]
			last.lines = append(last.lines, raw)
			cont.feed(raw)
			continue
		}
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, ctrlPrefix); ok {
			kind, d, err := parseCtrl(rest)
			if err != nil {
				return nil, fmt.Errorf("%w (line %d)", err, i+1)
			}
			switch kind {
			case "delay":
				next.delay, delaySet = d, true
			case "pause":
				next.pause = d
			case "handover":
				next.handover = true
			}
			ctrlAt = i + 1
			continue
		}
		next.lines = []string{raw}
		s.commands = append(s.commands, next)
		next, delaySet, ctrlAt = command{delay: defaultDelay}, false, 0
		cont = continuation{}
		cont.feed(raw)
	}

	if cont.open() {
		return nil, fmt.Errorf("%w (line %d)", errUnterminated, len(lines))
	}
	// With nothing left to type, a pause still means something: hold the last
	// prompt that long before the session is ended. The others don't.
	if delaySet || next.handover {
		return nil, fmt.Errorf("%w (line %d)", errDangling, ctrlAt)
	}
	s.pause = next.pause
	return s, nil
}

// parseCtrl parses the text after a control line's "#$".
func parseCtrl(text string) (kind string, d time.Duration, err error) {
	tokens := strings.Fields(text)
	if len(tokens) == 0 {
		return "", 0, errUnknownCtrl
	}
	switch kind = tokens[0]; kind {
	case "delay", "pause":
		d, err = millis(tokens[1:])
		return kind, d, err
	case "handover":
		return kind, 0, nil
	default:
		return "", 0, errUnknownCtrl
	}
}

// millis parses a control command's single argument, in milliseconds. The
// bound keeps a typo from either typing instantly (a negative delay) or
// hanging the recording for absurdly long; it is checked before the value is
// ever turned into a Duration, so a huge argument can't overflow one either.
func millis(opts []string) (time.Duration, error) {
	if len(opts) == 0 {
		return 0, errNoArgs
	}
	ms, err := strconv.ParseInt(opts[0], 10, 64)
	if err != nil {
		return 0, errBadArg
	}
	if ms < 0 || ms > int64(maxDelay/time.Millisecond) {
		return 0, fmt.Errorf("%w: must be between 0 and %s", errArgRange, maxDelay)
	}
	return time.Millisecond * time.Duration(ms), nil
}

// continuation tracks whether the command being read is still going: inside
// one or more heredocs, with a quote left open, or after a line ending in a
// backslash.
type continuation struct {
	heredocs  []heredocMark // pending delimiters, read in order: body then terminator, each in turn
	quote     byte          // the open quote, if any
	ansi      bool          // the open quote (always '\'' when set) is a `$'...'` one
	backslash bool          // the last line ended in an unescaped backslash
}

func (c *continuation) open() bool {
	return len(c.heredocs) > 0 || c.quote != 0 || c.backslash
}

// feed reads one more line of the command. Heredoc bodies start once the
// command line itself is complete: a quote or backslash carrying it on to the
// next line comes first, as bash reads it.
func (c *continuation) feed(line string) {
	if len(c.heredocs) > 0 && c.quote == 0 && !c.backslash {
		next := c.heredocs[0]
		term := line
		if next.dash {
			term = strings.TrimLeft(term, "\t")
		}
		if term == next.delim {
			c.heredocs = c.heredocs[1:]
		}
		return
	}
	sc := scanLine(line, c.quote, c.ansi)
	c.quote, c.ansi, c.backslash = sc.quote, sc.ansi, sc.backslash
	c.heredocs = append(c.heredocs, heredocIn(line, sc.literal)...)
}

// lineScan is what scanLine makes of a line of shell.
type lineScan struct {
	literal   []bool // per byte: text rather than syntax -- inside quotes, or in a comment
	quote     byte   // the quote still open at the end of the line, if any
	ansi      bool   // the open quote (always '\'' when set) is a `$'...'` one
	backslash bool   // the line ends in an unescaped backslash
}

// scanLine walks a line of shell starting inside quote q (0 for none, ansi
// saying whether that quote is a `$'...'` one), tracking quotes and backslash
// escapes the way bash reads them. An unquoted `#` that starts a word --
// after whitespace, or after a metacharacter that already ended the previous
// word -- begins a comment, which runs to the end of the line.
func scanLine(line string, q byte, ansi bool) lineScan {
	lit := make([]bool, len(line))
	esc, dollar := false, false // dollar: the previous byte was a live, unquoted '$'
	for i := 0; i < len(line); i++ {
		c := line[i]
		live := false
		switch {
		case esc:
			esc = false
			lit[i] = q != 0
		case q == '\'':
			lit[i] = true
			switch {
			case ansi && c == '\\':
				esc = true
			case c == '\'':
				q, ansi = 0, false
			}
		case q == '"':
			lit[i] = true
			switch c {
			case '"':
				q = 0
			case '\\':
				esc = true
			}
		case c == '\\':
			esc = true
		case c == '\'':
			lit[i] = true
			q, ansi = c, dollar
		case c == '"':
			lit[i] = true
			q = c
		case c == '#' && (i == 0 || strings.IndexByte(commentBefore, line[i-1]) >= 0):
			for ; i < len(line); i++ {
				lit[i] = true
			}
			return lineScan{literal: lit, quote: q, ansi: ansi}
		default:
			live = c == '$' && !dollar // `$$` is one token, and opens no `$'...'`
		}
		dollar = live
	}
	return lineScan{literal: lit, quote: q, ansi: ansi, backslash: esc}
}

// commentBefore are the bytes after which an unquoted `#` starts a new word,
// and so a comment: whitespace, or a metacharacter that already ends one.
const commentBefore = " \t;|&()"

// heredocMark is one pending heredoc: the delimiter its body ends at, and
// whether the `<<-` form was used.
type heredocMark struct {
	delim string
	dash  bool
}

// heredocOpenMark matches the `<<` or `<<-` that opens a heredoc, up to but
// not including the delimiter word -- heredocWord reads that separately, with
// its own quoting rules.
var heredocOpenMark = regexp.MustCompile(`<<(-?)[ \t]*`)

// heredocMeta are the shell metacharacters that end an unquoted heredoc
// delimiter word, the way whitespace does.
const heredocMeta = "|&;()<>"

// heredocDelim reports the delimiter a line opens a heredoc with, and whether
// the `<<-` form was used. A line can open more than one heredoc; this is the
// first.
func heredocDelim(line string) (delim string, dash bool) {
	marks := heredocIn(line, scanLine(line, 0, false).literal)
	if len(marks) == 0 {
		return "", false
	}
	return marks[0].delim, marks[0].dash
}

// heredocIn is heredocDelim given the line's scan, which the caller may know
// better than a fresh one would (a quote can be open from a previous line),
// and reports every heredoc the line opens, left to right.
func heredocIn(line string, literal []bool) []heredocMark {
	var marks []heredocMark
	for _, m := range heredocOpenMark.FindAllStringSubmatchIndex(line, -1) {
		if m[0] > 0 && line[m[0]-1] == '<' {
			continue // `<<<` is a here-string, not a heredoc
		}
		if literal[m[0]] {
			continue // `echo "a << b"` is text, not a redirection
		}
		if delim, ok := heredocWord(line[m[1]:]); ok {
			marks = append(marks, heredocMark{delim, m[3] > m[2]})
		}
	}
	return marks
}

// heredocWord reads a heredoc delimiter from the text right after `<<`: one
// bash word, running to the next unescaped whitespace or shell metacharacter,
// with quoted runs taken verbatim and a backslash escaping the byte after it
// -- so `EO"F"`, `"EOF"x` and `E\ OF` are EOF, EOFx and "E OF".
func heredocWord(s string) (word string, ok bool) {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '"' || c == '\'':
			end := strings.IndexByte(s[i+1:], c)
			if end < 0 {
				return "", false
			}
			b.WriteString(s[i+1 : i+1+end])
			i += end + 2
		case c == '\\' && i+1 < len(s):
			b.WriteByte(s[i+1])
			i += 2
		case c == '\\' || c == ' ' || c == '\t' || strings.IndexByte(heredocMeta, c) >= 0:
			i = len(s)
		default:
			b.WriteByte(c)
			i++
		}
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}
