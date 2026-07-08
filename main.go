package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/creack/pty"
	flags "github.com/jessevdk/go-flags"
)

// CtrlPrefix marks a control line in a script, e.g. "#$ delay 100".
const CtrlPrefix = "#$"

var (
	ErrUnknownCtrl = errors.New("unknown control command")
	ErrNoArgs      = errors.New("no arguments given to command")
	ErrBadArg      = errors.New("invalid command argument")
)

// Options is the command-line configuration, parsed by go-flags.
type Options struct {
	Cols   int     `long:"cols" description:"terminal width in columns (default: current terminal)"`
	Rows   int     `long:"rows" description:"terminal height in rows (default: current terminal)"`
	Settle int     `long:"settle" default:"2000" description:"ms to wait for asciinema to warm up before typing"`
	Wait   int     `long:"wait" default:"100" description:"ms to sleep between commands (#$ wait overrides per section)"`
	Speed  float64 `long:"speed" default:"1.0" description:"typing speed multiplier (2 = twice as fast; scales #$ delay)"`
	Quiet  bool    `short:"q" long:"quiet" description:"do not echo the recorded session to this terminal"`
	Human  bool    `long:"human" description:"type like a human -- digraph-aware jittered timing and pauses"`
	Seed   *int64  `long:"seed" description:"rng seed for --human (default: random each run, printed on start)"`

	Args struct {
		Script  string `positional-arg-name:"script" description:"script to type"`
		Outfile string `positional-arg-name:"outfile" description:"output .cast file"`
	} `positional-args:"yes" required:"yes"`
}

func main() {
	log.SetFlags(0)

	var opts Options
	parser := flags.NewParser(&opts, flags.Default)
	parser.Usage = "[OPTIONS]"
	if _, err := parser.Parse(); err != nil {
		if flags.WroteHelp(err) {
			os.Exit(0)
		}
		os.Exit(1) // go-flags already printed the error
	}

	if _, err := exec.LookPath("asciinema"); err != nil {
		log.Fatal("can't find asciinema executable on PATH")
	}

	s, err := NewScript(opts.Args.Script)
	if err != nil {
		log.Fatal("parsing script failed: ", err)
	}

	if err := s.Run(&opts); err != nil {
		log.Fatal(err)
	}
}

// Command is an action to be run against a running Script.
type Command interface {
	Run(*Script)
}

// Shell is a line of input to type into the recorded shell.
type Shell struct {
	Cmd string
}

// NewShell creates a new Shell, ensuring the line ends in a newline so it runs.
func NewShell(cmd string) Shell {
	if !strings.HasSuffix(cmd, "\n") {
		cmd += "\n"
	}
	return Shell{Cmd: cmd}
}

// Run types the command one rune at a time, pausing Delay between keystrokes.
func (s Shell) Run(sc *Script) {
	base := sc.base()
	var ks []keystroke
	if sc.human != nil {
		ks = sc.human.plan(s.Cmd, base)
	} else {
		ks = uniform(s.Cmd, base)
	}
	for _, k := range ks {
		if _, err := io.WriteString(sc.pty, k.data); err != nil {
			log.Fatal("writing to pty failed: ", err)
		}
		time.Sleep(k.pause)
	}
}

// keystroke is one unit of typing: bytes to write, then a pause. It's the
// interface between the human-typing subsystem (which plans them) and Shell.Run
// (which replays them).
type keystroke struct {
	data  string
	pause time.Duration
}

// uniform types each rune with the same delay -- the default, machine-steady
// typing used when --human is off.
func uniform(line string, delay time.Duration) []keystroke {
	ks := make([]keystroke, 0, len(line))
	for _, r := range line {
		ks = append(ks, keystroke{string(r), delay})
	}
	return ks
}

// Wait changes the interval between subsequent commands.
type Wait struct {
	Duration time.Duration
}

func NewWait(opts []string) (Wait, error) {
	if len(opts) == 0 {
		return Wait{}, ErrNoArgs
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(opts[0]), 10, 64)
	if err != nil {
		return Wait{}, ErrBadArg
	}
	return Wait{Duration: time.Millisecond * time.Duration(ms)}, nil
}

func (w Wait) Run(s *Script) { s.Wait = w.Duration }

// Delay changes the typing speed (interval between keystrokes) of subsequent commands.
type Delay struct {
	Interval time.Duration
}

func NewDelay(opts []string) (Delay, error) {
	if len(opts) == 0 {
		return Delay{}, ErrNoArgs
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(opts[0]), 10, 64)
	if err != nil {
		return Delay{}, ErrBadArg
	}
	return Delay{Interval: time.Millisecond * time.Duration(ms)}, nil
}

func (d Delay) Run(sc *Script) { sc.Delay = d.Interval }

// NewCtrl parses a control command (the text after the "#$" prefix).
func NewCtrl(cmd string) (Command, error) {
	tokens := strings.Split(cmd, " ")
	switch strings.TrimSpace(tokens[0]) {
	case "delay":
		return NewDelay(tokens[1:])
	case "wait":
		return NewWait(tokens[1:])
	default:
		return nil, ErrUnknownCtrl
	}
}

// Script is a parsed sequence of commands to type into a recorded session.
type Script struct {
	Commands []Command
	Delay    time.Duration // between keystrokes
	Wait     time.Duration // between commands

	speed float64        // typing-speed multiplier applied to Delay (1 = as written)
	pty   io.WriteCloser // pty master; keystrokes get written here
	human *human         // non-nil when --human is on; drives natural typing
}

// base is the effective per-keystroke delay: the current Delay scaled by the
// speed multiplier (higher speed -> shorter delay).
func (s *Script) base() time.Duration {
	if s.speed > 0 {
		return time.Duration(float64(s.Delay) / s.speed)
	}
	return s.Delay
}

// NewScript parses a Script from the file at path.
func NewScript(path string) (*Script, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseScript(string(b))
}

// parseScript parses a Script from raw script text.
func parseScript(text string) (*Script, error) {
	s := &Script{
		Delay: time.Millisecond * 40,
		Wait:  time.Millisecond * 100,
		speed: 1.0,
	}

	for i, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, CtrlPrefix) {
			ctrl, err := NewCtrl(strings.TrimSpace(line[len(CtrlPrefix):]))
			if err != nil {
				return nil, fmt.Errorf("%w (line %d)", err, i+1)
			}
			s.Commands = append(s.Commands, ctrl)
		} else {
			s.Commands = append(s.Commands, NewShell(line))
		}
	}

	return s, nil
}

// bashRC is a minimal bash config for demos: a coloured prompt and nothing else
// -- no user dotfiles, no surprises.
const bashRC = `PS1='\[\e[1;34m\]\w\[\e[0m\]\$ '` + "\n"

// bashCommand builds the command asciinema runs: a clean bash that loads a
// minimal coloured prompt from a temp rcfile (no user dotfiles) and silences
// the macOS deprecation banner. The returned cleanup removes the temp rcfile
// once recording ends.
func bashCommand() (cmd string, cleanup func(), err error) {
	cleanup = func() {}

	f, err := os.CreateTemp("", "asciiscript-bashrc-*")
	if err != nil {
		return "", cleanup, fmt.Errorf("couldn't create bash rcfile: %w", err)
	}
	if _, err := f.WriteString(bashRC); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", cleanup, fmt.Errorf("couldn't write bash rcfile: %w", err)
	}
	f.Close()
	cleanup = func() { os.Remove(f.Name()) }
	return "env BASH_SILENCE_DEPRECATION_WARNING=1 bash --noprofile --rcfile " + f.Name(), cleanup, nil
}

var oscColorQuery = regexp.MustCompile(`\x1b\]([0-9;]+);\?(\x07|\x1b\\)`)

// stripQueries removes terminal capability queries from buf before it's echoed
// to the live terminal. asciinema queries the terminal (colour palette, cursor
// position, ...) on startup; if those reach the real terminal it replies, and
// the reply then sits unread in the tty input buffer and spills onto the shell
// prompt after recording ends. Stripping them from the mirror keeps the real
// terminal silent. (Queries split across reads may slip through -- rare, as
// they land in one burst at startup.)
func stripQueries(buf []byte) []byte {
	buf = oscColorQuery.ReplaceAll(buf, nil)
	for _, q := range [][]byte{
		[]byte("\x1b[6n"),
		[]byte("\x1b[0c"), []byte("\x1b[c"),
		[]byte("\x1b[>0c"), []byte("\x1b[>c"),
		[]byte("\x1b[?2026$p"),
		[]byte("\x1b[?u"),
	} {
		buf = bytes.ReplaceAll(buf, q, nil)
	}
	return buf
}

// termSize resolves the recording window size. A dimension set on the command
// line wins; any left at 0 is filled from the current terminal (stdin), falling
// back to 80x24 when stdin isn't a terminal (e.g. piped or backgrounded).
func termSize(o *Options) (cols, rows uint16) {
	cols, rows = uint16(o.Cols), uint16(o.Rows)
	if cols != 0 && rows != 0 {
		return cols, rows
	}
	dc, dr := uint16(80), uint16(24)
	if ws, err := pty.GetsizeFull(os.Stdin); err == nil && ws.Cols > 0 && ws.Rows > 0 {
		dc, dr = ws.Cols, ws.Rows
	}
	if cols == 0 {
		cols = dc
	}
	if rows == 0 {
		rows = dr
	}
	return cols, rows
}

// Run records the script to outfile by hosting `asciinema rec` inside a pty and
// injecting keystrokes into it. asciinema sees a real terminal, so it runs
// interactively and forwards our keystrokes to the shell it records.
func (s *Script) Run(o *Options) error {
	// the flag sets the starting inter-command wait; `#$ wait` overrides it per
	// section as the script runs.
	s.Wait = time.Duration(o.Wait) * time.Millisecond

	if o.Speed <= 0 {
		return fmt.Errorf("--speed must be greater than 0 (got %g)", o.Speed)
	}
	s.speed = o.Speed

	if o.Human {
		seed := time.Now().UnixNano()
		if o.Seed != nil {
			seed = *o.Seed
		}
		fmt.Fprintf(os.Stderr, "asciiscript: human typing (seed %d)\n", seed)
		s.human = newHuman(seed)
	}

	shellCmd, cleanup, err := bashCommand()
	if err != nil {
		return err
	}
	defer cleanup()

	cmd := exec.Command(
		"asciinema", "rec",
		"--quiet", // suppress asciinema's own !!!/::: diagnostics
		"--overwrite",
		"--command", shellCmd,
		o.Args.Outfile,
	)

	cols, rows := termSize(o)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return fmt.Errorf("couldn't start recording: %w", err)
	}
	s.pty = ptmx
	defer ptmx.Close()

	// Drain asciinema's output so the pty never blocks, and mirror it live
	// unless quiet -- with startup terminal queries stripped so their replies
	// don't leak onto the prompt afterwards. This is asciinema's own terminal
	// output, not the .cast (which asciinema writes to outfile).
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 && !o.Quiet {
				os.Stdout.Write(stripQueries(buf[:n]))
			}
			if err != nil {
				return
			}
		}
	}()

	// Give asciinema time to warm up before typing. The shell prints its prompt
	// almost immediately, but asciinema doesn't start forwarding our keystrokes
	// to it for ~1-2s after launch -- type sooner and the first commands land in
	// the void and never make it into the recording. Watching for the prompt
	// doesn't help (it shows up long before input forwarding is live), so this
	// is a plain fixed wait. Bump -settle if early lines still get dropped.
	time.Sleep(time.Duration(o.Settle) * time.Millisecond)

	for _, c := range s.Commands {
		c.Run(s)
		time.Sleep(s.Wait)
	}

	// End the shell -> asciinema stops and flushes the recording.
	if _, err := io.WriteString(ptmx, "exit\n"); err != nil {
		return fmt.Errorf("couldn't send exit: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("asciinema exited with error: %w", err)
	}
	return nil
}
