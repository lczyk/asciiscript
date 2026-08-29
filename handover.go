package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/term"
)

// keyboard carries the real terminal's input into the recorded session while a
// handover is live, and drops it the rest of the time -- a stray keypress during
// automated typing is not something the recording should contain.
//
// Reading starts with the first handover and never stops: a Read already parked
// on the terminal can't be called off, so anything else would leave a keystroke
// stranded in a goroutine, to surface at whatever moment it eventually returns.
type keyboard struct {
	in io.Reader

	mu      sync.Mutex
	started bool
	to      io.Writer // where input goes, nil between handovers
}

func (k *keyboard) pump() {
	buf := make([]byte, 256)
	for {
		n, err := k.in.Read(buf)
		if n > 0 {
			k.mu.Lock()
			if k.to != nil {
				_, _ = k.to.Write(buf[:n])
			}
			k.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// lend routes the keyboard to w until the returned func takes it back.
func (k *keyboard) lend(w io.Writer) func() {
	k.mu.Lock()
	first := !k.started
	k.started = true
	k.to = w
	k.mu.Unlock()
	if first {
		go k.pump()
	}
	return func() {
		k.mu.Lock()
		k.to = nil
		k.mu.Unlock()
	}
}

// rawStdin is the default script.raw: raw mode means the keys a handed-over
// command wants -- ctrl-o, ctrl-x, arrows -- reach it as themselves rather than
// being buffered into lines or turned into signals, and stops the terminal
// echoing input that the recorded session is already echoing back.
func (s *script) rawStdin() (func(), error) {
	fd := int(os.Stdin.Fd())
	prev, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("couldn't put the terminal in raw mode: %w", err)
	}
	return func() { _ = term.Restore(fd, prev) }, nil
}

// lendTerminal types nothing and waits for nobody: the person running the recording
// drives the command that was just typed, and the script resumes when they land
// back at a prompt. There is no deadline -- the wait is on a human.
func (s *script) lendTerminal(before int) error {
	restore, err := s.raw()
	if err != nil {
		return err
	}
	defer restore()

	back := s.keys.lend(s.pty)
	defer back()

	for s.mon.marked() <= before {
		if err := s.sleep(syncPoll); err != nil {
			return err
		}
	}
	return nil
}

// checkHandover rejects the settings a `#$ handover` script can't work under.
// Both are silent traps otherwise: the person driving would be typing into a
// session they can't see, or one nothing is listening on.
func checkHandover(wants bool, o *options) error {
	if !wants {
		return nil
	}
	switch {
	case o.Quiet:
		return errors.New("`#$ handover` needs the session on screen to be driven, so it can't be recorded with --quiet")
	case !term.IsTerminal(int(os.Stdin.Fd())):
		return errors.New("`#$ handover` needs a keyboard to hand over to, and stdin isn't a terminal")
	}
	return nil
}
