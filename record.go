//go:build !windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// promptMarker is an invisible tag the recorded shell's prompts carry so a run
// can tell when the shell is ready for the next line instead of guessing at how
// long a command takes. It rides in an OSC 133 "command finished" sequence:
// terminals that implement semantic prompts read it as one (and get the exit
// status for free), the rest ignore it, and either way it occupies no columns.
//
// The token is per run because the sequence also ends up in the .cast, and a
// recording of asciiscript replayed inside asciiscript would otherwise announce
// prompts that never happened. It comes off the seed rather than a second rng,
// so a take pinned with --seed carries the same marker as the one it repeats.
type promptMarker struct {
	probe string         // plain-text tag inside the sequence
	strip *regexp.Regexp // the whole sequence as the shell emits it
}

func newPromptMarker(seed int64) promptMarker {
	return promptMarkerFor(fmt.Sprintf("%016x", uint64(seed)))
}

// promptMarkerFor builds the marker for a given token. Split out from
// newPromptMarker so tests get a marker they can write into their inputs.
func promptMarkerFor(token string) promptMarker {
	probe := "asciiscript=" + token
	return promptMarker{
		probe: probe,
		strip: regexp.MustCompile(`\x1b\]133;D;[0-9]*;` + regexp.QuoteMeta(probe) + `\x07`),
	}
}

// prefix is the marker as a bash prompt fragment. \[ \] keeps it out of
// readline's width accounting, so it can't shift the cursor or wrap a line.
func (p promptMarker) prefix() string {
	return `\[\e]133;D;$?;` + p.probe + `\a\]`
}

// promptPwd abbreviates the working directory for the prompt the way fish's
// prompt_pwd does: ~ for home, every parent cut to its first letter (a dot and
// a letter for a dotdir), the last component in full -- ~/g/asciiscript.
// Written for bash 3.2, which macOS ships.
const promptPwd = `__asciiscript_pwd() {
	local p=$PWD out='' seg
	case $p in
		"$HOME") p='~' ;;
		"$HOME"/*) p="~${p#"$HOME"}" ;;
	esac
	while [[ $p == */* ]]; do
		seg=${p%%/*} p=${p#*/}
		[[ $seg == .?* ]] && out+=${seg:0:2}/ || out+=${seg:0:1}/
	done
	printf '%s' "$out$p"
}
`

// bashRC is a minimal bash config for demos: a coloured prompt and nothing else
// -- no user dotfiles, no surprises. PS2 carries the marker as well as PS1, so
// continuation lines (heredoc bodies, trailing backslashes) are waited on like
// any other line rather than sitting out the timeout. Both are read-only: a
// script that sets its own prompt would otherwise silently take the marker
// with it, and every line after would wait out the whole timeout.
func bashRC(m promptMarker) string {
	return promptPwd +
		"PS1='" + m.prefix() + `\[\e[1;34m\]$(__asciiscript_pwd)\[\e[0m\]\$ ` + "'\n" +
		"PS2='" + m.prefix() + `> ` + "'\n" +
		"readonly PS1 PS2\n"
}

// writeRC puts the rcfile for the recorded shell in a temp file. The returned
// cleanup removes it once recording ends.
func writeRC(m promptMarker) (path string, cleanup func(), err error) {
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
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

var oscColorQuery = regexp.MustCompile(`\x1b\]([0-9;]+);\?(\x07|\x1b\\)`)

// stripQueries removes terminal capability queries from buf before it's echoed
// to the live terminal. A program the script runs may ask the terminal about
// itself (colour palette, cursor position, ...); if the query reaches the real
// terminal it replies, and the reply then sits unread in the tty input buffer
// and spills onto the shell prompt after recording ends. Stripping them from
// the mirror keeps the real terminal silent. (A query split across reads may
// slip through.)
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

// mirror drains the recorded shell's output so the pty never blocks, writes it
// into the recording, echoes it live unless quiet, and counts the prompts
// going by.
type mirror struct {
	quiet bool
	mark  promptMarker // prompt marker to tally
	cast  *castWriter  // the recording; nil in tests that only count prompts

	// lent is set while the terminal is handed over. Queries then pass
	// through to the real terminal, whose replies the keyboard is forwarding;
	// the rest of the time nothing would forward them, so they're stripped.
	lent atomic.Bool

	mu    sync.Mutex
	tail  []byte // unmatched trailing bytes of the last read, in case a marker straddles two
	marks int
}

const mirrorTailMax = 64 // longer than any marker's prefix, so a split one is still found

func (m *mirror) run(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			// The recording is written before the prompt count moves, so a
			// line typed on seeing a prompt lands after the output that showed it.
			m.mu.Lock()
			if m.cast != nil {
				_ = m.cast.output(buf[:n]) // remembered by the writer, reported by record
			}
			m.tally(buf[:n])
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

// tally counts the prompt markers in buf -- whole sequences only, since the
// probe text on its own is what `echo "$PS1"` prints. Reads split wherever the
// pty happens to fill, so what follows the last match is carried over and
// rescanned with the next; nothing already matched is, so nothing counts twice.
// Called with m.mu held.
func (m *mirror) tally(buf []byte) {
	b := make([]byte, 0, len(m.tail)+len(buf))
	b = append(b, m.tail...)
	b = append(b, buf...)
	end := 0
	for _, loc := range m.mark.strip.FindAllIndex(b, -1) {
		m.marks++
		end = loc[1]
	}
	keep := b[max(end, len(b)-mirrorTailMax):]
	m.tail = append(m.tail[:0], keep...)
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
	if !m.lent.Load() {
		buf = stripQueries(buf)
	}
	return m.mark.strip.ReplaceAll(buf, nil)
}

const (
	startTimeout = 5 * time.Second        // how long the recorded shell gets to show its first prompt
	killGrace    = 2 * time.Second        // shutdown budget after an interrupt
	drainGrace   = 500 * time.Millisecond // how long the last output gets to come through once the shell has exited
	syncPoll     = 20 * time.Millisecond  // how often to check for a new prompt
)

// session is a recording in progress: the pty the shell runs in, the timing
// model, the mirror of the session's output, the recording being written,
// and the seams that let the tests drive all of it without a terminal.
type session struct {
	speed  float64         // typing-speed multiplier applied to the script's timing (1 = as written)
	pty    io.WriteCloser  // pty master; keystrokes get written here
	jitter *jitter         // plans the human-like keystroke timing
	mon    *mirror         // the shell's output, watched for prompts
	cast   *castWriter     // the recording; nil in tests that don't look at it
	done   <-chan struct{} // closed on SIGINT/SIGTERM/SIGHUP

	// captureInput records the keystrokes the script types as input events.
	// Never a handover's: those are a person's.
	captureInput bool

	// cmdTimeout is how long a typed line gets to finish before typing carries
	// on anyway.
	cmdTimeout time.Duration
	warn       io.Writer // where warnings go; never the pty, which is being recorded
	keys       *keyboard // the real terminal's input, on loan during a handover

	// raw puts the real terminal into raw mode for the duration of a handover
	// and returns the undo. Tests swap it out for one without a terminal.
	raw func() (func(), error)

	// resize sizes the recording to the real terminal, for a handover in
	// progress when that terminal changes shape. Nil without a terminal.
	resize func()

	// sleep waits between keystrokes and commands, giving up early if the run
	// is interrupted. Tests swap it out to replay a script without waiting.
	sleep func(time.Duration) error
}

func newSession() *session {
	s := &session{
		speed:  1,
		jitter: newJitter(0, 0),
		mon:    &mirror{},
		warn:   os.Stderr,
		keys:   &keyboard{in: os.Stdin},
	}
	s.sleep = s.realSleep
	s.raw = s.rawStdin
	return s
}

// realSleep is the default session.sleep: sleep d, or abort if a signal lands first.
func (s *session) realSleep(d time.Duration) error {
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

// scaled applies --speed to a duration from the script (higher speed -> shorter).
func (s *session) scaled(d time.Duration) time.Duration {
	if s.speed > 0 {
		return time.Duration(float64(d) / s.speed)
	}
	return d
}

// typeLine types a line and the newline that runs it, by replaying the timing
// model's plan: sleep out each planned pause, then write its key. The pause
// belongs before its keystroke -- that is the gap the model computed it for.
// before, if given, runs once the first pause is over and the first key is
// about to go in.
func (s *session) typeLine(line string, delay, pause time.Duration, before func()) error {
	for i, k := range s.jitter.plan(line+"\n", s.scaled(delay), s.scaled(pause)) {
		if err := s.sleep(k.pause); err != nil {
			return err
		}
		if i == 0 && before != nil {
			before()
		}
		if _, err := io.WriteString(s.pty, k.data); err != nil {
			return fmt.Errorf("writing to pty failed: %w", err)
		}
		if s.captureInput && s.cast != nil {
			_ = s.cast.input([]byte(k.data))
		}
	}
	return nil
}

// awaitPrompt blocks until the recorded shell has printed more prompts than
// before -- until the line just typed has finished, in other words. before is
// the count from just before that line was typed, so the prompt already on
// screen at the time doesn't satisfy the wait. With a timeout the wait gives
// up after that long and reports false; without one it waits as long as it
// takes.
func (s *session) awaitPrompt(before int, timeout time.Duration) (bool, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for s.mon.marked() <= before {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return false, nil
		}
		if err := s.sleep(syncPoll); err != nil {
			return false, err
		}
	}
	return true, nil
}

// syncPrompt waits for the line just typed to finish. A command that never
// returns to a prompt of its own accord -- an editor, a pager, anything reading
// stdin -- can't be waited for, because the input that would end it is the
// input being held back. Those run the timeout out, say so, and get typed over
// anyway.
func (s *session) syncPrompt(line string, before int) error {
	ok, err := s.awaitPrompt(before, s.cmdTimeout)
	if err == nil && !ok {
		fmt.Fprintf(s.warn,
			"asciiscript: %q hasn't finished after %s -- typing on regardless; if it holds the terminal (an editor, a pager) put `#$ handover` in front of it\n",
			line, s.cmdTimeout)
	}
	return err
}

// typeAll waits for the recorded shell's first prompt -- the rcfile is loaded
// and readline is listening -- then types every command in order, each line
// waited on before the next. A command's `#$ pause` gets a marker in the
// recording, placed where the typing resumes.
func (s *session) typeAll(sc *script) error {
	ok, err := s.awaitPrompt(0, startTimeout)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("the recorded shell showed no prompt within %s", startTimeout)
	}
	for _, c := range sc.commands {
		if c.handover {
			fmt.Fprintln(s.warn, "asciiscript: the next command is yours -- the script picks up again once it drops you back at a prompt")
		}
		for j, line := range c.lines {
			if s.cast != nil {
				if err := s.cast.failed(); err != nil {
					return err
				}
			}
			// The pause the script asked for goes before the command, so
			// before its first line only; the rest get the model's line gap.
			var pause time.Duration
			var mark func()
			if j == 0 {
				pause = c.pause
				if pause > 0 && s.cast != nil {
					mark = func() { _ = s.cast.marker(line) }
				}
			}
			before := s.mon.marked()
			if err := s.typeLine(line, c.delay, pause, mark); err != nil {
				return err
			}
			var err error
			if c.handover && j == len(c.lines)-1 {
				err = s.lendTerminal(before)
			} else {
				err = s.syncPrompt(line, before)
			}
			if err != nil {
				return err
			}
		}
	}
	// A trailing pause holds the last prompt before the session is ended.
	if sc.pause > 0 {
		return s.sleep(s.jitter.linePause(0, s.scaled(sc.pause)))
	}
	return nil
}

// finish ends the recorded session and reports how the shell exited. The
// shell is sent end-of-input rather than a typed `exit`, so the recording
// ends the way a session does. It is only a byte on the pty, though: whatever
// holds the terminal's input at that moment eats it, so a pager, an
// unterminated quote or any command still reading stdin leaves the shell alive
// and the wait unbounded. Hence the deadline and the kill behind it.
//
// The shell's last output can still be on its way once it has exited; drained
// closes when the mirror has read the pty to its end, which happens at once on
// macOS and on Linux only once nothing holds the pty open any more -- so it
// gets a moment and no more.
func finish(cmd *exec.Cmd, w io.Writer, drained <-chan struct{}, timeout time.Duration) (status int, err error) {
	// A failed write means the pty is already gone, so there is nothing left to
	// end -- but the shell still has to be reaped rather than left behind.
	_, werr := io.WriteString(w, "\x04")

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-waited:
		if werr != nil {
			err = fmt.Errorf("couldn't end the session: %w", werr)
		}
	case <-t.C:
		_ = cmd.Process.Kill()
		<-waited
		err = fmt.Errorf("the shell didn't exit within %s of the session being ended -- something was still holding the terminal (a pager, an unterminated quote, an interactive command, or whatever was running when the take was interrupted)", timeout)
	}

	select {
	case <-drained:
	case <-time.After(drainGrace):
	}
	return exitStatus(cmd), err
}

// exitStatus is how the shell ended, the way $? would have it: the exit code,
// or 128 plus the signal that killed it.
func exitStatus(cmd *exec.Cmd) int {
	ps := cmd.ProcessState
	if ps == nil {
		return 1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// randomSeed picks the seed for a take: four digits, short enough to read off
// the screen and type back in as --seed.
func randomSeed() int64 {
	return rand.Int64N(10000)
}

// record records the script to outfile: it runs bash in a pty, types the
// script into it, and writes what comes back as an asciicast.
func record(sc *script, o *options) error {
	if err := o.validate(); err != nil {
		return err
	}
	s := newSession()
	s.speed = o.Speed
	s.captureInput = o.CaptureInput

	handover := sc.hasHandover()
	if err := checkHandover(handover, o, term.IsTerminal(int(os.Stdin.Fd()))); err != nil {
		return err
	}
	seed := randomSeed()
	if o.Seed != nil {
		seed = *o.Seed
	}
	if o.Jitter > 0 {
		// the seed only matters when jitter is active; print it so a good take
		// can be reproduced with --seed.
		fmt.Fprintf(s.warn, "asciiscript: jitter %g (seed %d)\n", o.Jitter, seed)
	}
	s.jitter = newJitter(o.Jitter, seed)

	// A signal has to unwind through the defers below: killed halfway, the run
	// would leave the temp rcfile behind and the shell still running. SIGHUP
	// is the terminal window closing, which is the same thing.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
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

	marker := newPromptMarker(seed)
	s.cmdTimeout = time.Duration(o.CmdTimeout) * time.Millisecond
	s.mon.mark = marker

	bash, err := exec.LookPath("bash")
	if err != nil {
		return errors.New("can't find bash on PATH")
	}
	rc, cleanup, err := writeRC(marker)
	if err != nil {
		return err
	}
	defer cleanup()

	out, err := os.Create(o.Args.Outfile)
	if err != nil {
		return fmt.Errorf("couldn't create the recording: %w", err)
	}
	defer out.Close()
	discard := func() { os.Remove(o.Args.Outfile) } // a header with no session behind it is no recording

	cols, rows := termSize(o, handover)
	if handover {
		if ws, err := pty.GetsizeFull(os.Stdin); err == nil && (ws.Cols != cols || ws.Rows != rows) {
			fmt.Fprintf(s.warn, "asciiscript: recording at %dx%d but this terminal is %dx%d -- a handed-over command will draw itself to the wrong size\n", cols, rows, ws.Cols, ws.Rows)
		}
	}

	cast, err := newCastWriter(out, castHeader{
		Term:          castTerm{Cols: cols, Rows: rows, Type: os.Getenv("TERM")},
		Timestamp:     time.Now().Unix(),
		IdleTimeLimit: o.IdleTimeLimit,
		Command:       "asciiscript " + filepath.Base(o.Args.Script),
		Title:         o.Title,
		Env:           &castEnv{Shell: bash},
	}, time.Now)
	if err != nil {
		discard()
		return err
	}
	s.cast = cast
	s.mon.cast = cast

	// Without readline the tty echoes what's typed and the terminal wraps it
	// wherever it's played back; readline's own redraw of a wrapped line writes
	// the recording's width into the file. A script has no use for readline.
	cmd := exec.Command(bash, "--noprofile", "--noediting", "--rcfile", rc)
	cmd.Env = append(os.Environ(), "BASH_SILENCE_DEPRECATION_WARNING=1")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		discard()
		return fmt.Errorf("couldn't start recording: %w", err)
	}
	s.pty = ptmx
	defer ptmx.Close()
	if handover {
		s.resize = func() {
			if pty.InheritSize(os.Stdin, ptmx) != nil {
				return
			}
			if ws, err := pty.GetsizeFull(os.Stdin); err == nil {
				_ = cast.resize(ws.Cols, ws.Rows)
			}
		}
	}

	s.mon.quiet = o.Quiet
	drained := make(chan struct{})
	go func() {
		s.mon.run(ptmx)
		close(drained)
	}()

	typed := s.typeAll(sc)

	// Stop the recording either way: on an interrupt or a half-typed script,
	// the shell still has to be ended and what it printed written out.
	grace := time.Duration(o.ExitTimeout) * time.Millisecond
	if typed != nil {
		grace = killGrace
	}
	status, stopped := finish(cmd, ptmx, drained, grace)
	_ = cast.exit(status)
	return errors.Join(typed, stopped, cast.close())
}
