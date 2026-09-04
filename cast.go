//go:build !windows

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// castHeader is the first line of an asciicast v3 file. A struct rather than a
// map so the keys come out in this order every time, and every optional field
// stays out of the file when unset.
type castHeader struct {
	Version       int      `json:"version"`
	Term          castTerm `json:"term"`
	Timestamp     int64    `json:"timestamp,omitempty"`
	IdleTimeLimit float64  `json:"idle_time_limit,omitempty"`
	Command       string   `json:"command,omitempty"`
	Title         string   `json:"title,omitempty"`
	Env           *castEnv `json:"env,omitempty"`
}

type castTerm struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
	Type string `json:"type,omitempty"`
}

type castEnv struct {
	Shell string `json:"SHELL,omitempty"`
}

// castWriter writes an asciicast v3 file: the header, then one JSON array per
// event with the interval since the previous one. Intervals are quantised to
// the millisecond the way asciinema does it, carrying the rounding error over
// to the next event so the written times track the real ones. Output is fed
// in whatever pieces the pty hands over; a UTF-8 sequence cut between two of
// them is held back until it is whole. Safe to use from several goroutines.
type castWriter struct {
	w   *bufio.Writer
	now func() time.Time // the clock; tests swap it out

	mu     sync.Mutex
	epoch  time.Time     // what event times are measured from
	last   time.Duration // unquantised time of the previous event
	carry  int64         // quantisation error, in ns, owed to the next interval
	tail   []byte        // the start of a UTF-8 sequence the last output cut short
	err    error         // the first write error; every call after it fails the same way
	ended  bool          // the exit event is written, and nothing may follow it
	closed bool
}

const castQuantum = int64(time.Millisecond)

// newCastWriter writes the header to w and starts the clock.
func newCastWriter(w io.Writer, h castHeader, now func() time.Time) (*castWriter, error) {
	c := &castWriter{w: bufio.NewWriter(w), now: now}
	h.Version = 3
	enc := json.NewEncoder(c.w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(h); err != nil {
		return nil, fmt.Errorf("writing the recording's header: %w", err)
	}
	c.epoch = c.now()
	return c, nil
}

// output records bytes the session printed.
func (c *castWriter) output(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || c.closed || c.ended {
		return c.err
	}
	buf := make([]byte, 0, len(c.tail)+len(b))
	buf = append(buf, c.tail...)
	buf = append(buf, b...)
	whole, tail := splitIncompleteRune(buf)
	if text := sanitiseUTF8(whole); text != "" {
		c.event('o', text)
	}
	c.tail = append(c.tail[:0], tail...)
	return c.err
}

// input records keystrokes typed into the session.
func (c *castWriter) input(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || c.closed || c.ended {
		return c.err
	}
	c.event('i', sanitiseUTF8(b))
	return c.err
}

// marker records a point a player can jump to; the label may be empty.
func (c *castWriter) marker(label string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || c.closed || c.ended {
		return c.err
	}
	c.event('m', sanitiseUTF8([]byte(label)))
	return c.err
}

// resize records a change of the terminal's size.
func (c *castWriter) resize(cols, rows uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || c.closed || c.ended {
		return c.err
	}
	c.event('r', fmt.Sprintf("%dx%d", cols, rows))
	return c.err
}

// exit records the session's exit status. It is the last event: any
// half-received UTF-8 sequence is flushed before it, as the replacement
// character it would have become, and whatever arrives after it is dropped.
func (c *castWriter) exit(status int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || c.closed || c.ended {
		return c.err
	}
	c.flushTail()
	c.event('x', strconv.Itoa(status))
	c.ended = true
	return c.err
}

// close flushes everything to the underlying writer. Whatever the writer wraps
// is the caller's to close.
func (c *castWriter) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.err
	}
	c.closed = true
	if c.err == nil {
		c.flushTail()
	}
	if err := c.w.Flush(); err != nil && c.err == nil {
		c.err = fmt.Errorf("writing the recording: %w", err)
	}
	return c.err
}

// failed reports the first write error, if any.
func (c *castWriter) failed() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// flushTail writes out a UTF-8 sequence that never got completed. Called with
// c.mu held.
func (c *castWriter) flushTail() {
	if len(c.tail) > 0 {
		c.event('o', sanitiseUTF8(c.tail))
		c.tail = c.tail[:0]
	}
}

// event writes one event line, laid out as asciinema lays out its own, stamped
// with the interval since the previous event. Called with c.mu held.
func (c *castWriter) event(code byte, data string) {
	if c.err != nil {
		return
	}
	at := max(c.now().Sub(c.epoch), c.last)
	ms := c.quantise(at - c.last)
	c.last = at
	line := []byte{'['}
	line = strconv.AppendInt(line, ms/1000, 10)
	line = append(line, '.', byte('0'+ms/100%10), byte('0'+ms/10%10), byte('0'+ms%10))
	line = append(line, ',', ' ', '"', code, '"', ',', ' ')
	line = appendJSONString(line, data)
	line = append(line, ']', '\n')
	if _, err := c.w.Write(line); err != nil {
		c.err = fmt.Errorf("writing the recording: %w", err)
	}
}

// quantise rounds an interval to whole milliseconds, carrying the error over
// to the next call so that the rounded intervals never drift from the real
// ones by more than half a millisecond. Given d >= 0, which event sees to, the
// result is too: the carry is at most half a quantum either way.
func (c *castWriter) quantise(d time.Duration) int64 {
	corrected := int64(d) + c.carry
	steps := (corrected + castQuantum/2) / castQuantum
	c.carry = corrected - steps*castQuantum
	return steps
}

// splitIncompleteRune cuts off the start of a multi-byte UTF-8 sequence at the
// end of b that the rest of the bytes haven't arrived for yet.
func splitIncompleteRune(b []byte) (whole, tail []byte) {
	for k := 1; k <= utf8.UTFMax-1 && k <= len(b); k++ {
		if s := b[len(b)-k:]; !utf8.FullRune(s) {
			whole, tail = b[:len(b)-k], s
		}
	}
	if tail == nil {
		return b, nil
	}
	return whole, tail
}

// sanitiseUTF8 replaces what isn't valid UTF-8 with U+FFFD, one per invalid
// sequence rather than per byte: a truncated sequence is one mistake, and
// counting its bytes separately would say otherwise.
func sanitiseUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	out := make([]byte, 0, len(b)+8)
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			out = append(out, "\uFFFD"...)
			b = b[invalidLen(b):]
			continue
		}
		out = append(out, b[:size]...)
		b = b[size:]
	}
	return string(out)
}

// invalidLen is the length of the invalid sequence at the start of b: a lead
// byte and however many of the continuation bytes it calls for are there and
// in range -- the "maximal subpart" the Unicode standard says to replace as
// one -- or a single byte when b doesn't start with a lead byte at all.
func invalidLen(b []byte) int {
	lead := b[0]
	var want int
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		want = 2
	case lead == 0xE0:
		want, lo = 3, 0xA0
	case lead >= 0xE1 && lead <= 0xEC, lead == 0xEE, lead == 0xEF:
		want = 3
	case lead == 0xED:
		want, hi = 3, 0x9F
	case lead == 0xF0:
		want, lo = 4, 0x90
	case lead >= 0xF1 && lead <= 0xF3:
		want = 4
	case lead == 0xF4:
		want, hi = 4, 0x8F
	default:
		return 1
	}
	n := 1
	for n < want && n < len(b) && b[n] >= lo && b[n] <= hi {
		n++
		lo, hi = 0x80, 0xBF
	}
	return n
}

// appendJSONString appends s as a JSON string the way asciinema writes them:
// the C0 controls escaped (the short forms for \n, \r and \t), the quote and
// the backslash escaped, and everything else -- DEL, <, >, &, multi-byte
// characters -- as it is. s must be valid UTF-8.
func appendJSONString(dst []byte, s string) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			dst = append(dst, '\\', c)
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\r':
			dst = append(dst, '\\', 'r')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '"')
}
