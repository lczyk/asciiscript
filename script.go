package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ctrlPrefix marks a control line in a script, e.g. "#$ delay 100".
const ctrlPrefix = "#$"

var (
	errUnknownCtrl = errors.New("unknown control command")
	errNoArgs      = errors.New("no arguments given to command")
	errBadArg      = errors.New("invalid command argument")
	errInterrupted = errors.New("interrupted")
)

// command is an action to be run against a running script.
type command interface {
	run(*script) error
}

// shell is a line of input to type into the recorded shell.
type shell struct {
	cmd string
}

// newShell creates a new shell, ensuring the line ends in a newline so it runs.
func newShell(cmd string) shell {
	if !strings.HasSuffix(cmd, "\n") {
		cmd += "\n"
	}
	return shell{cmd: cmd}
}

// Run types the command by replaying the jitter subsystem's keystroke plan:
// wait out the planned pause, then write the bytes. The pause belongs before
// its keystroke -- that is the gap the timing model computed it for.
func (s shell) run(sc *script) error {
	for _, k := range sc.jitter.plan(s.cmd, sc.base()) {
		if err := sc.sleep(k.pause); err != nil {
			return err
		}
		if _, err := io.WriteString(sc.pty, k.data); err != nil {
			return fmt.Errorf("writing to pty failed: %w", err)
		}
	}
	return nil
}

// setWait changes the pause between subsequent commands.
type setWait struct{ d time.Duration }

func (w setWait) run(s *script) error { s.wait = w.d; return nil }

// setDelay changes the typing speed (interval between keystrokes) of subsequent commands.
type setDelay struct{ d time.Duration }

func (d setDelay) run(s *script) error { s.delay = d.d; return nil }

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

// handover gives the next line to whoever is running the recording: it gets
// typed as usual, and then the real keyboard is wired to the recorded session
// until the command they were handed ends. For the commands nothing else can
// drive -- an editor, a REPL, anything wanting a keypress -- and the session
// records what they do exactly as if it had been typed by the script.
type handover struct{}

func (h handover) run(s *script) error {
	s.armed = true
	fmt.Fprintln(s.warn, "asciiscript: the next command is yours -- the script picks up again once it drops you back at a prompt")
	return nil
}

// newCtrl parses a control command (the text after the "#$" prefix).
func newCtrl(cmd string) (command, error) {
	tokens := strings.Fields(cmd)
	if len(tokens) == 0 {
		return nil, errUnknownCtrl
	}
	switch tokens[0] {
	case "delay":
		d, err := millis(tokens[1:])
		return setDelay{d}, err
	case "wait":
		d, err := millis(tokens[1:])
		return setWait{d}, err
	case "handover":
		return handover{}, nil
	default:
		return nil, errUnknownCtrl
	}
}

// script is a parsed sequence of commands to type into a recorded session.
type script struct {
	commands []command
	delay    time.Duration // between keystrokes
	wait     time.Duration // between commands

	speed  float64         // typing-speed multiplier applied to delay (1 = as written)
	pty    io.WriteCloser  // pty master; keystrokes get written here
	jitter *jitter         // plans the human-like keystroke timing
	mon    *mirror         // asciinema's output, watched for our own keystrokes
	done   <-chan struct{} // closed on SIGINT/SIGTERM

	// cmdTimeout is how long a typed line gets to finish before typing carries
	// on anyway.
	cmdTimeout time.Duration
	warn       io.Writer // where warnings go; never the pty, which is being recorded

	keys  *keyboard // the real terminal's input, on loan during a handover
	armed bool      // set by `#$ handover`, spent by the next line typed

	// raw puts the real terminal into raw mode for the duration of a handover
	// and returns the undo. Tests swap it out for one without a terminal.
	raw func() (func(), error)

	// sleep waits between keystrokes and commands, giving up early if the run
	// is interrupted. Tests swap it out to replay a script without waiting.
	sleep func(time.Duration) error
}

// realSleep is the default script.sleep: sleep d, or abort if a signal lands first.
func (s *script) realSleep(d time.Duration) error {
	if d <= 0 {
		select {
		case <-s.done:
			return errInterrupted
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.done:
		return errInterrupted
	case <-t.C:
		return nil
	}
}

// base is the effective per-keystroke delay: the current delay scaled by the
// speed multiplier (higher speed -> shorter delay).
func (s *script) base() time.Duration {
	if s.speed > 0 {
		return time.Duration(float64(s.delay) / s.speed)
	}
	return s.delay
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
func parseScript(text string) (*script, error) {
	s := &script{
		delay: time.Millisecond * 40,
		wait:  time.Millisecond * 100,
		speed: 1.0,
		mon:   &mirror{},
		warn:  os.Stderr,
		keys:  &keyboard{in: os.Stdin},
	}
	s.sleep = s.realSleep
	s.raw = s.rawStdin

	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // the newline the file ends with, not a blank line
	}

	// Inside a heredoc every line is literal input for the command that opened
	// it -- blank lines included, "#$" lines included -- so the usual skipping
	// and control-line parsing is suspended until the delimiter shows up.
	var heredoc string
	var heredocDash bool

	for i, line := range lines {
		if heredoc != "" {
			s.commands = append(s.commands, newShell(line))
			term := line
			if heredocDash {
				term = strings.TrimLeft(term, "\t")
			}
			if term == heredoc {
				heredoc = ""
			}
			continue
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ctrlPrefix) {
			ctrl, err := newCtrl(strings.TrimSpace(line[len(ctrlPrefix):]))
			if err != nil {
				return nil, fmt.Errorf("%w (line %d)", err, i+1)
			}
			s.commands = append(s.commands, ctrl)
			continue
		}
		s.commands = append(s.commands, newShell(line))
		heredoc, heredocDash = heredocDelim(line)
	}

	return s, nil
}

var heredocStart = regexp.MustCompile(`<<(-?)[ \t]*(?:"([^"]*)"|'([^']*)'|([A-Za-z_][A-Za-z0-9_]*))`)

// heredocDelim reports the delimiter a line opens a heredoc with, and whether
// the `<<-` form was used (which lets the terminator be indented with tabs).
func heredocDelim(line string) (delim string, dash bool) {
	inQuotes := quoted(line)
	for _, m := range heredocStart.FindAllStringSubmatchIndex(line, -1) {
		if m[0] > 0 && line[m[0]-1] == '<' {
			continue // `<<<` is a here-string, not a heredoc
		}
		if inQuotes[m[0]] {
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

// quoted marks the bytes of line that sit inside single or double quotes.
func quoted(line string) []bool {
	in := make([]bool, len(line))
	var q byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if q != 0 {
			in[i] = true
			if c == q {
				q = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			q = c
		}
	}
	return in
}

// hasHandover reports whether the script hands the terminal over at any point.
func (s *script) hasHandover() bool {
	for _, c := range s.commands {
		if _, ok := c.(handover); ok {
			return true
		}
	}
	return false
}
