// Command typer is a typing-capture harness. It shows a series of prompts and
// records exactly how you type each one -- every keystroke with its timing,
// including typos and the backspaces that fix them -- to a JSONL log. That log
// is the raw material for tuning asciiscript's --human typing model.
//
// Usage:
//
//	go run ./cmd/typer -o typing-log.jsonl
//
// Type each line and press Enter. It's a real line editor -- backspace,
// forward-delete, and cursor movement (arrows, home, end) all work, so you can
// arrow back and insert just like normal. Type naturally -- don't slow down or
// "perform"; the whole point is your real rhythm, mistakes and edits included.
// Ctrl-C quits early (whatever you've finished is saved).
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/christopher-dG/asciiscript/internal/typinglog"
)

// prompts span the character classes the --human model cares about: short
// commands, paths, quotes/punctuation, digits, symbols, mixed case, and one
// prose line for natural rhythm.
var prompts = []string{
	"ls -la",
	"cd ~/projects/asciiscript",
	`git commit -m "fix: handle empty input"`,
	`grep -rn "TODO" ./src`,
	`echo "hello, world!"`,
	"for i in 1 2 3; do echo $i; done",
	"docker run --rm -it ubuntu:22.04 bash",
	"The quick brown fox jumps over the lazy dog.",
	"curl -sSL https://example.com/install.sh | sh",
	"SELECT * FROM users WHERE id = 42;",
}

const (
	reset = "\x1b[0m"
	dim   = "\x1b[2m"
	green = "\x1b[32m"
	red   = "\x1b[31m"
)

func main() {
	out := flag.String("o", "typing-log.jsonl", "output log file (JSONL)")
	list := flag.Bool("list", false, "print the prompts and exit")
	flag.Parse()

	if *list {
		for _, p := range prompts {
			fmt.Println(p)
		}
		return
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fmt.Fprintln(os.Stderr, "typer needs an interactive terminal (run it directly, not piped)")
		os.Exit(1)
	}

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "can't open output:", err)
		os.Exit(1)
	}
	defer f.Close()

	old, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "can't enter raw mode:", err)
		os.Exit(1)
	}
	defer term.Restore(fd, old)

	in := bufio.NewReader(os.Stdin)
	w := os.Stdout

	fmt.Fprintf(w, "typer -- type each line and press Enter. Backspace, arrows, home/end all work.\r\n")
	fmt.Fprintf(w, "type naturally, mistakes and all. Ctrl-C quits early.\r\n")

	done := 0
	for i, target := range prompts {
		rec, quit := capture(in, w, target, i+1, len(prompts))
		if rec.Events != nil {
			if err := typinglog.Write(f, rec); err != nil {
				term.Restore(fd, old)
				fmt.Fprintln(os.Stderr, "\r\nwrite failed:", err)
				os.Exit(1)
			}
			f.Sync()
			done++
		}
		if quit {
			break
		}
	}

	term.Restore(fd, old)
	fmt.Fprintf(w, "\r\nsaved %d/%d prompts to %s\n", done, len(prompts), *out)
}

// capture records one prompt as a small line editor: it supports insertion at
// the cursor, backspace, forward-delete, and cursor movement (arrows, home,
// end), echoing everything itself (raw mode has no echo) with correct chars
// green and typos red. Every keystroke is logged with its position and timing.
// Returns the record and whether the user asked to quit (Ctrl-C).
func capture(in *bufio.Reader, w io.Writer, target string, n, total int) (typinglog.Record, bool) {
	tr := []rune(target)

	fmt.Fprintf(w, "\r\n[%d/%d]\r\n  %s%s%s\r\n", n, total, dim, target, reset)

	var (
		buf    []rune
		cur    int // cursor index in buf (0..len)
		events []typinglog.Event
		start  = time.Now()
		last   = start
	)

	// log appends an event stamped relative to start and the previous key.
	log := func(k typinglog.Kind, pos int, r string, expected string) {
		now := time.Now()
		events = append(events, typinglog.Event{
			TUS:      now.Sub(start).Microseconds(),
			DTUS:     now.Sub(last).Microseconds(),
			Rune:     r,
			Kind:     k,
			Pos:      pos,
			Expected: expected,
		})
		last = now
	}

	redraw(w, buf, cur, tr)

	for {
		r, _, err := in.ReadRune()
		if err != nil {
			break
		}

		switch {
		case r == 3: // Ctrl-C -> quit
			return finish(target, buf, events, start), true

		case r == '\r' || r == '\n': // Enter -> submit
			return finish(target, buf, events, start), false

		case r == 27: // escape sequence (arrows, home, end, delete)
			intro, _, err := in.ReadRune()
			if err != nil || (intro != '[' && intro != 'O') {
				continue // bare Esc or unknown -- ignore
			}
			code, _, err := in.ReadRune()
			if err != nil {
				continue
			}
			switch code {
			case 'D': // left
				if cur > 0 {
					log(typinglog.Left, cur, "", "")
					cur--
				}
			case 'C': // right
				if cur < len(buf) {
					log(typinglog.Right, cur, "", "")
					cur++
				}
			case 'H': // home
				log(typinglog.Home, cur, "", "")
				cur = 0
			case 'F': // end
				log(typinglog.End, cur, "", "")
				cur = len(buf)
			case '1', '3', '4', '7', '8': // ESC [ n ~  (home/delete/end variants)
				tilde, _, err := in.ReadRune()
				if err != nil || tilde != '~' {
					continue
				}
				switch code {
				case '3': // forward delete
					if cur < len(buf) {
						log(typinglog.Delete, cur, string(buf[cur]), "")
						buf = append(buf[:cur], buf[cur+1:]...)
					}
				case '1', '7': // home
					log(typinglog.Home, cur, "", "")
					cur = 0
				case '4', '8': // end
					log(typinglog.End, cur, "", "")
					cur = len(buf)
				}
			}
			redraw(w, buf, cur, tr)

		case r == 127 || r == 8: // backspace -> delete before cursor
			if cur == 0 {
				continue
			}
			log(typinglog.Backspace, cur, string(buf[cur-1]), "")
			buf = append(buf[:cur-1], buf[cur:]...)
			cur--
			redraw(w, buf, cur, tr)

		case r >= 32: // printable -> insert at cursor
			kind := typinglog.Typo
			expected := ""
			if cur < len(tr) && r == tr[cur] {
				kind = typinglog.Correct
			} else if cur < len(tr) {
				expected = string(tr[cur])
			}
			log(kind, cur, string(r), expected)
			buf = append(buf, 0)
			copy(buf[cur+1:], buf[cur:])
			buf[cur] = r
			cur++
			redraw(w, buf, cur, tr)
		}
	}

	return finish(target, buf, events, start), false
}

// redraw repaints the input line: "> " then buf (green where it matches the
// target, red otherwise), then parks the cursor at its index.
func redraw(w io.Writer, buf []rune, cur int, tr []rune) {
	fmt.Fprint(w, "\r\x1b[K> ")
	for i, r := range buf {
		colour := green
		if i >= len(tr) || r != tr[i] {
			colour = red
		}
		fmt.Fprintf(w, "%s%c%s", colour, r, reset)
	}
	fmt.Fprintf(w, "\r\x1b[%dC", 2+cur) // 2 = len("> ")
}

func finish(target string, buf []rune, events []typinglog.Event, start time.Time) typinglog.Record {
	final := string(buf)
	return typinglog.Record{
		Target:        target,
		StartedUnixMS: start.UnixMilli(),
		DurationUS:    time.Since(start).Microseconds(),
		Final:         final,
		Matched:       final == target,
		Events:        events,
	}
}
