package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	ErrInterrupted = errors.New("interrupted")
)

// Options is the command-line configuration, parsed by go-flags.
type Options struct {
	Cols    int     `long:"cols" description:"terminal width in columns (default: current terminal)"`
	Rows    int     `long:"rows" description:"terminal height in rows (default: current terminal)"`
	Settle  int     `long:"settle" default:"2000" description:"ms to wait for asciinema to warm up before typing"`
	Wait    int     `long:"wait" default:"100" description:"ms to pause after each command finishes (#$ wait overrides per section)"`
	Speed   float64 `long:"speed" default:"1.0" description:"typing speed multiplier (2 = twice as fast; scales #$ delay)"`
	Quiet   bool    `short:"q" long:"quiet" description:"do not echo the recorded session to this terminal"`
	Jitter  float64 `long:"jitter" default:"1.0" description:"human-jitter scale (1 = human-like, 0 = uniform/off)"`
	Timeout int     `long:"timeout" default:"10000" description:"ms to wait for asciinema to stop after the script ends"`
	NoSync  bool    `long:"no-sync" description:"type the next command without waiting for the previous one to finish"`
	CmdSync int     `long:"cmd-timeout" default:"600000" description:"ms to wait for a command to finish before typing on regardless"`
	Seed    *int64  `long:"seed" description:"rng seed for --jitter (default: random each run, printed on start)"`

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
	Run(*Script) error
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

// Run types the command by replaying the jitter subsystem's keystroke plan:
// wait out the planned pause, then write the bytes. The pause belongs before
// its keystroke -- that is the gap the timing model computed it for.
func (s Shell) Run(sc *Script) error {
	for _, k := range sc.jitter.plan(s.Cmd, sc.base()) {
		if err := sc.sleep(k.pause); err != nil {
			return err
		}
		if _, err := io.WriteString(sc.pty, k.data); err != nil {
			return fmt.Errorf("writing to pty failed: %w", err)
		}
	}
	return nil
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

func (w Wait) Run(s *Script) error { s.Wait = w.Duration; return nil }

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

func (d Delay) Run(sc *Script) error { sc.Delay = d.Interval; return nil }

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

	speed  float64         // typing-speed multiplier applied to Delay (1 = as written)
	pty    io.WriteCloser  // pty master; keystrokes get written here
	jitter *jitter         // plans the human-like keystroke timing
	mon    *mirror         // asciinema's output, watched for our own keystrokes
	done   <-chan struct{} // closed on SIGINT/SIGTERM

	// syncFor is how long a typed line gets to finish before typing carries on
	// anyway. Zero disables the wait, leaving Wait as the only spacing.
	syncFor time.Duration
	warn    io.Writer // where warnings go; never the pty, which is being recorded

	// sleep waits between keystrokes and commands, giving up early if the run
	// is interrupted. Tests swap it out to replay a script without waiting.
	sleep func(time.Duration) error
}

// wait is the default Script.sleep: sleep d, or abort if a signal lands first.
func (s *Script) wait(d time.Duration) error {
	if d <= 0 {
		select {
		case <-s.done:
			return ErrInterrupted
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.done:
		return ErrInterrupted
	case <-t.C:
		return nil
	}
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
		mon:   &mirror{},
		warn:  os.Stderr,
	}
	s.sleep = s.wait

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
			s.Commands = append(s.Commands, NewShell(line))
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
		if strings.HasPrefix(line, CtrlPrefix) {
			ctrl, err := NewCtrl(strings.TrimSpace(line[len(CtrlPrefix):]))
			if err != nil {
				return nil, fmt.Errorf("%w (line %d)", err, i+1)
			}
			s.Commands = append(s.Commands, ctrl)
			continue
		}
		s.Commands = append(s.Commands, NewShell(line))
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

// promptMarker is an invisible tag the recorded shell's prompts carry so a run
// can tell when the shell is ready for the next line instead of guessing at how
// long a command takes. It rides in an OSC 133 "command finished" sequence:
// terminals that implement semantic prompts read it as one (and get the exit
// status for free), the rest ignore it, and either way it occupies no columns.
//
// The token is unique per run because the sequence also ends up in the .cast,
// and a recording of asciiscript replayed inside asciiscript would otherwise
// announce prompts that never happened.
type promptMarker struct {
	probe string         // plain-text tag counted in the shell's output
	strip *regexp.Regexp // the whole sequence, for keeping the live echo clean
}

func newPromptMarker() (promptMarker, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return promptMarker{}, fmt.Errorf("couldn't generate a prompt marker: %w", err)
	}
	probe := "asciiscript=" + hex.EncodeToString(b)
	return promptMarker{
		probe: probe,
		strip: regexp.MustCompile(`\x1b\]133;D;[0-9]*;` + regexp.QuoteMeta(probe) + `\x07`),
	}, nil
}

// prefix is the marker as a bash prompt fragment. \[ \] keeps it out of
// readline's width accounting, so it can't shift the cursor or wrap a line.
func (p promptMarker) prefix() string {
	if p.probe == "" {
		return ""
	}
	return `\[\e]133;D;$?;` + p.probe + `\a\]`
}

// bashRC is a minimal bash config for demos: a coloured prompt and nothing else
// -- no user dotfiles, no surprises. PS2 carries the marker as well as PS1, so
// continuation lines (heredoc bodies, trailing backslashes) are waited on like
// any other line rather than sitting out the timeout.
func bashRC(m promptMarker) string {
	return "PS1='" + m.prefix() + `\[\e[1;34m\]\w\[\e[0m\]\$ ` + "'\n" +
		"PS2='" + m.prefix() + `> ` + "'\n"
}

// bashCommand builds the command asciinema runs: a clean bash that loads a
// minimal coloured prompt from a temp rcfile (no user dotfiles) and silences
// the macOS deprecation banner. The returned cleanup removes the temp rcfile
// once recording ends.
func bashCommand(m promptMarker) (cmd string, cleanup func(), err error) {
	cleanup = func() {}

	f, err := os.CreateTemp("", "asciiscript-bashrc-*")
	if err != nil {
		return "", cleanup, fmt.Errorf("couldn't create bash rcfile: %w", err)
	}
	if _, err := f.WriteString(bashRC(m)); err != nil {
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

// mirror drains asciinema's terminal output so the pty never blocks, echoes it
// live unless quiet, and keeps the opening bytes so a run can check that its
// own keystrokes are reaching the shell. This is asciinema's terminal output,
// not the .cast (which asciinema writes to outfile itself).
type mirror struct {
	quiet bool
	mark  promptMarker // prompt marker to tally; zero when syncing is off

	mu    sync.Mutex
	head  []byte
	tail  []byte // trailing bytes of the last read, in case a marker straddles two
	marks int
}

const mirrorHeadMax = 64 << 10

func (m *mirror) run(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			m.mu.Lock()
			m.tally(buf[:n])
			if len(m.head) < mirrorHeadMax {
				m.head = append(m.head, buf[:n]...)
			}
			m.mu.Unlock()
			// queries and prompt markers are stripped from the live echo only:
			// query replies would otherwise leak onto the prompt once recording
			// ends, and the markers are ours rather than the session's.
			if !m.quiet {
				os.Stdout.Write(m.clean(buf[:n]))
			}
		}
		if err != nil {
			return
		}
	}
}

// tally counts the prompt markers in buf. Reads split wherever the pty happens
// to fill, so the tail of each one is carried over and rescanned with the next;
// the carried tail is always shorter than a marker, so nothing counts twice.
// Called with m.mu held.
func (m *mirror) tally(buf []byte) {
	probe := []byte(m.mark.probe)
	if len(probe) == 0 {
		return
	}
	b := make([]byte, 0, len(m.tail)+len(buf))
	b = append(b, m.tail...)
	b = append(b, buf...)
	m.marks += bytes.Count(b, probe)
	m.tail = append(m.tail[:0], b[len(b)-min(len(probe)-1, len(b)):]...)
}

// marked reports how many prompts the recorded shell has printed so far.
func (m *mirror) marked() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.marks
}

// clean strips what belongs to asciiscript rather than the recorded session
// from a chunk bound for the live terminal. Only whole markers go: a marker
// split across two reads passes through intact, which the terminal handles
// fine, whereas half of one would leave a dangling escape sequence.
func (m *mirror) clean(buf []byte) []byte {
	buf = stripQueries(buf)
	if m.mark.strip != nil {
		buf = m.mark.strip.ReplaceAll(buf, nil)
	}
	return buf
}

// saw reports whether text has appeared in asciinema's output so far.
func (m *mirror) saw(text string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return bytes.Contains(m.head, []byte(text))
}

const (
	echoProbeLen = 8                      // short enough not to straddle a line wrap
	echoGrace    = 500 * time.Millisecond // how long the echo gets to come back
	killGrace    = 2 * time.Second        // shutdown budget after an interrupt
	syncPoll     = 20 * time.Millisecond  // how often to check for a new prompt
)

// echoProbe is the leading text of a typed line, used to spot that line's echo
// in asciinema's output. Empty when the line has nothing distinctive to match.
func echoProbe(line string) string {
	line = strings.TrimSuffix(line, "\n")
	if strings.TrimSpace(line) == "" {
		return ""
	}
	r := []rune(line)
	if len(r) > echoProbeLen {
		r = r[:echoProbeLen]
	}
	return string(r)
}

// confirmEcho waits for the first line typed to come back from the recorded
// shell. asciinema doesn't forward input for a second or two after launch, so
// too small a --settle means every keystroke lands in the void: the recording
// comes out empty and `exit` never arrives either. Checking the first line
// turns that into an error instead of a silent take.
func (s *Script) confirmEcho(line string, settle time.Duration) error {
	probe := echoProbe(line)
	if probe == "" {
		return nil
	}
	deadline := time.Now().Add(echoGrace)
	for !s.mon.saw(probe) {
		if time.Now().After(deadline) {
			return fmt.Errorf("the first command never echoed back -- asciinema wasn't ready for input yet; raise --settle (currently %s)", settle)
		}
		if err := s.sleep(20 * time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

// syncPrompt blocks until the recorded shell offers another prompt -- until the
// line just typed has finished, in other words. before is the marker count from
// just before that line was typed, so the prompt already on screen at the time
// doesn't satisfy the wait.
//
// A command that never returns to a prompt of its own accord -- an editor, a
// pager, anything reading stdin -- can't be waited for, because the input that
// would end it is the input being held back. Those run the timeout out, say so,
// and get typed over anyway, which is the fixed-wait behaviour --no-sync gives.
func (s *Script) syncPrompt(line string, before int) error {
	deadline := time.Now().Add(s.syncFor)
	for s.mon.marked() <= before {
		if time.Now().After(deadline) {
			fmt.Fprintf(s.warn,
				"asciiscript: %q hasn't finished after %s -- typing on regardless; if it holds the terminal (an editor, a pager) record with --no-sync\n",
				strings.TrimSuffix(line, "\n"), s.syncFor)
			return nil
		}
		if err := s.sleep(syncPoll); err != nil {
			return err
		}
	}
	return nil
}

// typeAll gives asciinema time to warm up, then runs every parsed command in
// order, waiting for each to finish and then pausing before the next. The shell
// prints its prompt almost immediately, but asciinema doesn't start forwarding
// keystrokes to it for ~1-2s after launch -- type sooner and the first commands
// land in the void and never make it into the recording. Watching for the
// prompt doesn't help (it shows up long before input forwarding is live), so
// the settle is a plain fixed wait and confirmEcho checks afterwards that it was
// long enough.
func (s *Script) typeAll(settle time.Duration) error {
	if err := s.sleep(settle); err != nil {
		return err
	}
	checked := false
	for _, c := range s.Commands {
		sh, typing := c.(Shell)
		var before int
		if typing {
			before = s.mon.marked()
		}
		if err := c.Run(s); err != nil {
			return err
		}
		if typing && !checked {
			checked = true
			if err := s.confirmEcho(sh.Cmd, settle); err != nil {
				return err
			}
		}
		if typing && s.syncFor > 0 {
			if err := s.syncPrompt(sh.Cmd, before); err != nil {
				return err
			}
		}
		if err := s.sleep(s.Wait); err != nil {
			return err
		}
	}
	return nil
}

// finish ends the recorded session and waits for asciinema to flush the .cast.
// `exit` is only bytes on the pty: whatever holds the terminal's input at that
// moment eats them, so a pager, an unterminated quote or any command still
// reading stdin leaves the shell alive and the wait unbounded. Hence the
// deadline and the kill behind it.
func finish(cmd *exec.Cmd, w io.Writer, timeout time.Duration) error {
	// A failed write means the pty is already gone, so there is nothing left to
	// exit -- but asciinema still has to be reaped rather than left behind.
	_, werr := io.WriteString(w, "exit\n")

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err := <-waited:
		switch {
		case werr != nil:
			return fmt.Errorf("couldn't send exit: %w", werr)
		case err != nil:
			return fmt.Errorf("asciinema exited with error: %w", err)
		}
		return nil
	case <-t.C:
		_ = cmd.Process.Kill()
		<-waited
		return fmt.Errorf("asciinema didn't stop within %s of `exit` -- the script probably left something holding the terminal (a pager, an unterminated quote, an interactive command)", timeout)
	}
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

	if o.Jitter < 0 {
		return fmt.Errorf("--jitter must be >= 0 (got %g)", o.Jitter)
	}
	if o.Settle < 0 {
		return fmt.Errorf("--settle must be >= 0 (got %d)", o.Settle)
	}
	if o.Timeout <= 0 {
		return fmt.Errorf("--timeout must be greater than 0 (got %d)", o.Timeout)
	}
	if o.CmdSync <= 0 {
		return fmt.Errorf("--cmd-timeout must be greater than 0 (got %d)", o.CmdSync)
	}
	seed := time.Now().UnixNano()
	if o.Seed != nil {
		seed = *o.Seed
	}
	if o.Jitter > 0 {
		// the seed only matters when jitter is active; print it so a good take
		// can be reproduced with --seed.
		fmt.Fprintf(os.Stderr, "asciiscript: jitter %g (seed %d)\n", o.Jitter, seed)
	}
	s.jitter = newJitter(o.Jitter, seed)

	// A signal has to unwind through the defers below: killed halfway, the run
	// would leave the temp rcfile behind and asciinema still recording.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	done := make(chan struct{})
	returned := make(chan struct{})
	defer close(returned)
	go func() {
		select {
		case <-sigs:
			close(done)
		case <-returned:
		}
	}()
	s.done = done

	// Without the wait there is no reason to mark the prompts, so --no-sync
	// leaves the recording free of them too.
	var marker promptMarker
	if !o.NoSync {
		m, err := newPromptMarker()
		if err != nil {
			return err
		}
		marker = m
		s.syncFor = time.Duration(o.CmdSync) * time.Millisecond
		s.mon.mark = marker
	}

	shellCmd, cleanup, err := bashCommand(marker)
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

	s.mon.quiet = o.Quiet
	go s.mon.run(ptmx)

	typed := s.typeAll(time.Duration(o.Settle) * time.Millisecond)

	// Stop the recording either way: on an interrupt or a half-typed script,
	// asciinema still has to be told to stop and flush what it has.
	grace := time.Duration(o.Timeout) * time.Millisecond
	if typed != nil {
		grace = killGrace
	}
	stopped := finish(cmd, ptmx, grace)

	if typed != nil {
		return typed
	}
	return stopped
}
