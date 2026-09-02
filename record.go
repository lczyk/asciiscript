//go:build !windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

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

// promptPwd is abbrevPwd as a bash function (bash 3.2, which macOS ships),
// for the prompt: a full \w from a deep directory wraps an 80-column recording.
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

// abbrevPwd shortens a working directory the way fish's prompt_pwd does: ~ for
// home, every parent cut to its first letter (a dot and a letter for a dotdir),
// the last component in full -- ~/g/asciiscript.
func abbrevPwd(path, home string) string {
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+"/"); ok && home != "" {
		path = "~/" + rest
	}
	parts := strings.Split(path, "/")
	for i, seg := range parts[:len(parts)-1] {
		keep := 1
		if strings.HasPrefix(seg, ".") {
			keep = 2
		}
		if r := []rune(seg); len(r) > keep {
			parts[i] = string(r[:keep])
		}
	}
	return strings.Join(parts, "/")
}

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

// rcfileVar carries the temp rcfile's path to the shell. asciinema runs its
// --command through `sh -c`, so a path spliced into the command string would
// need quoting; in the environment it needs none, and the .cast header's
// `command` field reads the same for every run instead of naming a temp file.
const rcfileVar = "ASCIISCRIPT_RCFILE"

// bashCommand builds the command asciinema runs -- a clean bash that loads a
// minimal coloured prompt from a temp rcfile (no user dotfiles) -- and the
// environment it needs: the rcfile's path, and the macOS deprecation banner
// silenced. The returned cleanup removes the temp rcfile once recording ends.
func bashCommand(m promptMarker) (cmd string, env []string, cleanup func(), err error) {
	cleanup = func() {}

	f, err := os.CreateTemp("", "asciiscript-bashrc-*")
	if err != nil {
		return "", nil, cleanup, fmt.Errorf("couldn't create bash rcfile: %w", err)
	}
	if _, err := f.WriteString(bashRC(m)); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, cleanup, fmt.Errorf("couldn't write bash rcfile: %w", err)
	}
	f.Close()
	cleanup = func() { os.Remove(f.Name()) }
	env = []string{"BASH_SILENCE_DEPRECATION_WARNING=1", rcfileVar + "=" + f.Name()}
	return `bash --noprofile --rcfile "$` + rcfileVar + `"`, env, cleanup, nil
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

	// lent is set while the terminal is handed over. Queries then pass
	// through to the real terminal, whose replies the keyboard is forwarding;
	// the rest of the time nothing would forward them, so they're stripped.
	lent atomic.Bool

	mu    sync.Mutex
	head  []byte
	tail  []byte // unmatched trailing bytes of the last read, in case a marker straddles two
	marks int
}

const (
	mirrorHeadMax = 64 << 10
	mirrorTailMax = 64 // longer than any marker's prefix, so a split one is still found
)

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

// seen is how much of asciinema's output has been kept so far; a point to
// look back from with sawAfter.
func (m *mirror) seen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.head)
}

// sawAfter reports whether text has appeared in asciinema's output since the
// point from.
func (m *mirror) sawAfter(text string, from int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return bytes.Contains(m.head[min(from, len(m.head)):], []byte(text))
}

const (
	echoProbeLen = 8                      // short enough not to straddle a line wrap
	echoGrace    = 500 * time.Millisecond // how long the first line's echo gets to come back
	startTimeout = 15 * time.Second       // how long the recorded shell gets to show its first prompt
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

// session is a recording in progress: the pty asciinema is hosted in, the
// timing model, the mirror of the session's output, and the seams that let
// the tests drive all of it without a terminal.
type session struct {
	speed  float64         // typing-speed multiplier applied to the script's timing (1 = as written)
	pty    io.WriteCloser  // pty master; keystrokes get written here
	jitter *jitter         // plans the human-like keystroke timing
	mon    *mirror         // asciinema's output, watched for our own keystrokes
	done   <-chan struct{} // closed on SIGINT/SIGTERM/SIGHUP

	// cmdTimeout is how long a typed line gets to finish before typing carries
	// on anyway; echoGrace how long the first line's echo gets to show up.
	cmdTimeout time.Duration
	echoGrace  time.Duration
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
		speed:     1,
		jitter:    newJitter(0, 0),
		mon:       &mirror{},
		echoGrace: echoGrace,
		warn:      os.Stderr,
		keys:      &keyboard{in: os.Stdin},
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
func (s *session) typeLine(line string, delay, pause time.Duration) error {
	for _, k := range s.jitter.plan(line+"\n", s.scaled(delay), s.scaled(pause)) {
		if err := s.sleep(k.pause); err != nil {
			return err
		}
		if _, err := io.WriteString(s.pty, k.data); err != nil {
			return fmt.Errorf("writing to pty failed: %w", err)
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

// confirmEcho waits for the first line typed to come back from the recorded
// shell, looking only at output from after the line went in. asciinema is
// forwarding input by the time the first prompt shows, but one that wasn't
// would swallow the whole take without a word -- the recording would come out
// empty and the shell never be told to end. Checking the first line turns
// that into an error instead of a silent take.
func (s *session) confirmEcho(line string, from int) error {
	probe := echoProbe(line)
	if probe == "" {
		return nil
	}
	deadline := time.Now().Add(s.echoGrace)
	for !s.mon.sawAfter(probe, from) {
		if time.Now().After(deadline) {
			return errors.New("the first line typed never echoed back -- asciinema took the keystrokes but didn't forward them to the shell")
		}
		if err := s.sleep(syncPoll); err != nil {
			return err
		}
	}
	return nil
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

// typeAll waits for the recorded shell's first prompt, then types every command
// in order, each line waited on before the next. The prompt is the signal that
// asciinema is up and forwarding input: type before it and the keystrokes
// either land in the void or come back echoed twice.
func (s *session) typeAll(sc *script) error {
	ok, err := s.awaitPrompt(0, startTimeout)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("the recorded shell showed no prompt within %s -- asciinema didn't start it", startTimeout)
	}
	for i, c := range sc.commands {
		if c.handover {
			fmt.Fprintln(s.warn, "asciiscript: the next command is yours -- the script picks up again once it drops you back at a prompt")
		}
		for j, line := range c.lines {
			// The pause the script asked for goes before the command, so
			// before its first line only; the rest get the model's line gap.
			var pause time.Duration
			if j == 0 {
				pause = c.pause
			}
			before, from := s.mon.marked(), s.mon.seen()
			if err := s.typeLine(line, c.delay, pause); err != nil {
				return err
			}
			if i == 0 && j == 0 {
				if err := s.confirmEcho(line, from); err != nil {
					return err
				}
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

// finish ends the recorded session and waits for asciinema to flush the .cast.
// The shell is sent end-of-input rather than a typed `exit`, so the recording
// ends the way a session does. It is only a byte on the pty, though: whatever
// holds the terminal's input at that moment eats it, so a pager, an
// unterminated quote or any command still reading stdin leaves the shell alive
// and the wait unbounded. Hence the deadline and the kill behind it.
func finish(cmd *exec.Cmd, w io.Writer, timeout time.Duration) error {
	// A failed write means the pty is already gone, so there is nothing left to
	// end -- but asciinema still has to be reaped rather than left behind.
	_, werr := io.WriteString(w, "\x04")

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err := <-waited:
		switch {
		case werr != nil:
			return fmt.Errorf("couldn't end the session: %w", werr)
		case err != nil:
			return fmt.Errorf("asciinema exited with error: %w", err)
		}
		return nil
	case <-t.C:
		_ = cmd.Process.Kill()
		<-waited
		return fmt.Errorf("asciinema didn't stop within %s of the session being ended -- something was still holding the terminal (a pager, an unterminated quote, an interactive command, or whatever was running when the take was interrupted)", timeout)
	}
}

// warnWideLines points out the script lines that will wrap at the recording's
// width, behind a prompt as wide as the one the take starts with. readline
// handles a wrap itself -- a carriage return and a redraw -- and that lands in
// the .cast, where it only renders right at the width it was recorded at.
// Counts runes, so a double-width character is undercounted.
func warnWideLines(sc *script, cols uint16, prompt string, w io.Writer) {
	limit := int(cols) - utf8.RuneCountInString(prompt)
	var wide []string
	for _, c := range sc.commands {
		for _, line := range c.lines {
			if utf8.RuneCountInString(line) > limit {
				wide = append(wide, line)
			}
		}
	}
	if len(wide) == 0 {
		return
	}
	more := ""
	if n := len(wide) - 1; n > 0 {
		more = fmt.Sprintf(" (and %d more)", n)
	}
	fmt.Fprintf(w, "asciiscript: %q is likely to wrap at %d columns%s -- a wrapped line plays back cleanly only at the width it was recorded at (asciinema play -r)\n",
		wide[0], cols, more)
}

// record records the script to outfile by hosting `asciinema rec` inside a pty
// and injecting keystrokes into it. asciinema sees a real terminal, so it runs
// interactively and forwards our keystrokes to the shell it records.
func record(sc *script, o *options) error {
	if err := o.validate(); err != nil {
		return err
	}
	s := newSession()
	s.speed = o.Speed

	handover := sc.hasHandover()
	if err := checkHandover(handover, o, term.IsTerminal(int(os.Stdin.Fd()))); err != nil {
		return err
	}
	seed := time.Now().UnixNano()
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
	// would leave the temp rcfile behind and asciinema still recording. SIGHUP
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

	shellCmd, env, cleanup, err := bashCommand(marker)
	if err != nil {
		return err
	}
	defer cleanup()

	args := []string{
		"rec",
		"--quiet", // suppress asciinema's own !!!/::: diagnostics
		"--overwrite",
		"--command", shellCmd,
	}
	if o.IdleTimeLimit > 0 {
		args = append(args, "--idle-time-limit", strconv.FormatFloat(o.IdleTimeLimit, 'f', -1, 64))
	}
	if o.Title != "" {
		args = append(args, "--title", o.Title)
	}
	args = append(args, o.Args.Outfile)
	cmd := exec.Command("asciinema", args...)
	cmd.Env = append(os.Environ(), env...)

	cols, rows := termSize(o, handover)
	if handover {
		if ws, err := pty.GetsizeFull(os.Stdin); err == nil && (ws.Cols != cols || ws.Rows != rows) {
			fmt.Fprintf(s.warn, "asciiscript: recording at %dx%d but this terminal is %dx%d -- a handed-over command will draw itself to the wrong size\n", cols, rows, ws.Cols, ws.Rows)
		}
	}
	if wd, err := os.Getwd(); err == nil {
		warnWideLines(sc, cols, abbrevPwd(wd, os.Getenv("HOME"))+"$ ", s.warn)
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return fmt.Errorf("couldn't start recording: %w", err)
	}
	s.pty = ptmx
	defer ptmx.Close()
	if handover {
		s.resize = func() { _ = pty.InheritSize(os.Stdin, ptmx) }
	}

	s.mon.quiet = o.Quiet
	go s.mon.run(ptmx)

	typed := s.typeAll(sc)

	// Stop the recording either way: on an interrupt or a half-typed script,
	// asciinema still has to be told to stop and flush what it has.
	grace := time.Duration(o.ExitTimeout) * time.Millisecond
	if typed != nil {
		grace = killGrace
	}
	return errors.Join(typed, finish(cmd, ptmx, grace))
}
