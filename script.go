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

var (
	errUnknownCtrl  = errors.New("unknown control command")
	errNoArgs       = errors.New("no arguments given to command")
	errBadArg       = errors.New("invalid command argument")
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

// millis parses a control command's single argument, in milliseconds.
func millis(opts []string) (time.Duration, error) {
	if len(opts) == 0 {
		return 0, errNoArgs
	}
	ms, err := strconv.ParseInt(opts[0], 10, 64)
	if err != nil {
		return 0, errBadArg
	}
	return time.Millisecond * time.Duration(ms), nil
}

// continuation tracks whether the command being read is still going: inside a
// heredoc, with a quote left open, or after a line ending in a backslash.
type continuation struct {
	heredoc   string // delimiter of the open heredoc, if any
	dash      bool   // the `<<-` form: the delimiter may be indented with tabs
	quote     byte   // the open quote, if any
	backslash bool   // the last line ended in an unescaped backslash
}

func (c *continuation) open() bool {
	return c.heredoc != "" || c.quote != 0 || c.backslash
}

// feed reads one more line of the command.
func (c *continuation) feed(line string) {
	if c.heredoc != "" {
		term := line
		if c.dash {
			term = strings.TrimLeft(term, "\t")
		}
		if term == c.heredoc {
			c.heredoc = ""
		}
		return
	}
	sc := scanLine(line, c.quote)
	c.quote, c.backslash = sc.quote, sc.backslash
	if c.quote == 0 {
		c.heredoc, c.dash = heredocIn(line, sc.literal)
	}
}

// lineScan is what scanLine makes of a line of shell.
type lineScan struct {
	literal   []bool // per byte: text rather than syntax -- inside quotes, or in a comment
	quote     byte   // the quote still open at the end of the line, if any
	backslash bool   // the line ends in an unescaped backslash
}

// scanLine walks a line of shell starting inside quote q (0 for none),
// tracking quotes and backslash escapes the way bash reads them. An unquoted
// `#` at a word start begins a comment, which runs to the end of the line.
func scanLine(line string, q byte) lineScan {
	lit := make([]bool, len(line))
	esc := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case esc:
			esc = false
			lit[i] = q != 0
		case q == '\'':
			lit[i] = true
			if c == '\'' {
				q = 0
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
		case c == '\'' || c == '"':
			lit[i] = true
			q = c
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			for ; i < len(line); i++ {
				lit[i] = true
			}
			return lineScan{literal: lit, quote: q}
		}
	}
	return lineScan{literal: lit, quote: q, backslash: esc}
}

var heredocStart = regexp.MustCompile(`<<(-?)[ \t]*(?:"([^"]*)"|'([^']*)'|([A-Za-z_][A-Za-z0-9_]*))`)

// heredocDelim reports the delimiter a line opens a heredoc with, and whether
// the `<<-` form was used.
func heredocDelim(line string) (delim string, dash bool) {
	return heredocIn(line, scanLine(line, 0).literal)
}

// heredocIn is heredocDelim given the line's scan, which the caller may know
// better than a fresh one would (a quote can be open from a previous line).
func heredocIn(line string, literal []bool) (delim string, dash bool) {
	for _, m := range heredocStart.FindAllStringSubmatchIndex(line, -1) {
		if m[0] > 0 && line[m[0]-1] == '<' {
			continue // `<<<` is a here-string, not a heredoc
		}
		if literal[m[0]] {
			continue // `echo "a << b"` is text, not a redirection
		}
		for _, g := range [][2]int{{m[4], m[5]}, {m[6], m[7]}, {m[8], m[9]}} {
			if g[0] >= 0 {
				return line[g[0]:g[1]], m[3] > m[2]
			}
		}
	}
	return "", false
}
