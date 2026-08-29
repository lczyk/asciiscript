package main

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

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

func newPromptMarker() promptMarker {
	return promptMarkerFor(fmt.Sprintf("%016x", rand.Uint64()))
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

// mirror drains asciinema's terminal output so the pty never blocks, echoes it
// live unless quiet, and keeps the opening bytes so a run can check that its
// own keystrokes are reaching the shell. This is asciinema's terminal output,
// not the .cast (which asciinema writes to outfile itself).
type mirror struct {
	quiet bool
	mark  promptMarker // prompt marker to tally

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
	return m.mark.strip.ReplaceAll(stripQueries(buf), nil)
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
func (s *script) confirmEcho(line string, settle time.Duration) error {
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
// and get typed over anyway.
func (s *script) syncPrompt(line string, before int) error {
	deadline := time.Now().Add(s.cmdTimeout)
	for s.mon.marked() <= before {
		if time.Now().After(deadline) {
			fmt.Fprintf(s.warn,
				"asciiscript: %q hasn't finished after %s -- typing on regardless; if it holds the terminal (an editor, a pager) put `#$ handover` in front of it\n",
				strings.TrimSuffix(line, "\n"), s.cmdTimeout)
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
func (s *script) typeAll(settle time.Duration) error {
	if err := s.sleep(settle); err != nil {
		return err
	}
	typed := false
	for _, c := range s.commands {
		sh, typing := c.(shell)

		// A control line is a note to asciiscript, not something the recorded
		// shell ever sees: it is never typed and never finishes, so it earns
		// no pause of its own. Because the pause belongs to the line it
		// precedes, `#$ wait N` takes effect on the very next line -- which is
		// where a script puts it to slow something down.
		if !typing {
			if err := c.run(s); err != nil {
				return err
			}
			continue
		}
		if typed {
			if err := s.sleep(s.wait); err != nil {
				return err
			}
		}

		before := s.mon.marked()
		if err := c.run(s); err != nil {
			return err
		}
		if !typed {
			typed = true
			if err := s.confirmEcho(sh.cmd, settle); err != nil {
				return err
			}
		}

		var err error
		if s.armed {
			s.armed = false
			err = s.lendTerminal(before)
		} else {
			err = s.syncPrompt(sh.cmd, before)
		}
		if err != nil {
			return err
		}
	}
	// One last pause, so the closing output gets a beat before `exit` lands.
	if typed {
		return s.sleep(s.wait)
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
func (s *script) record(o *options) error {
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
	if o.CmdTimeout <= 0 {
		return fmt.Errorf("--cmd-timeout must be greater than 0 (got %d)", o.CmdTimeout)
	}
	if o.ExitTimeout <= 0 {
		return fmt.Errorf("--exit-timeout must be greater than 0 (got %d)", o.ExitTimeout)
	}
	handover := s.hasHandover()
	if err := checkHandover(handover, o); err != nil {
		return err
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

	marker := newPromptMarker()
	s.cmdTimeout = time.Duration(o.CmdTimeout) * time.Millisecond
	s.mon.mark = marker

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
	if handover {
		if ws, err := pty.GetsizeFull(os.Stdin); err == nil && (ws.Cols != cols || ws.Rows != rows) {
			fmt.Fprintf(s.warn, "asciiscript: recording at %dx%d but this terminal is %dx%d -- a handed-over command will draw itself to the wrong size\n", cols, rows, ws.Cols, ws.Rows)
		}
	}
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
	grace := time.Duration(o.ExitTimeout) * time.Millisecond
	if typed != nil {
		grace = killGrace
	}
	stopped := finish(cmd, ptmx, grace)

	if typed != nil {
		return typed
	}
	return stopped
}
