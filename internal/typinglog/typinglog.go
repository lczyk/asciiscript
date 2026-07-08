// Package typinglog defines the on-disk schema for typing-capture sessions.
// The `typer` harness writes one Record per prompt as a line of JSON (JSONL);
// analysis/tuning tools read them back. Kept deliberately raw -- every
// keystroke with its timing and a light classification -- so the modelling
// (state machine, probabilities) happens downstream, not at capture time.
package typinglog

import (
	"bufio"
	"encoding/json"
	"io"
)

// Kind classifies a keystroke at the moment it was pressed, relative to the
// target text and the current cursor position.
type Kind string

const (
	Correct   Kind = "correct"   // a printable char inserted that matches target[pos]
	Typo      Kind = "typo"      // a printable char inserted that does not match target[pos]
	Backspace Kind = "backspace" // erase the char before the cursor
	Delete    Kind = "delete"    // forward-delete the char at the cursor
	Left      Kind = "left"      // cursor moved one left
	Right     Kind = "right"     // cursor moved one right
	Home      Kind = "home"      // cursor jumped to line start
	End       Kind = "end"       // cursor jumped to line end
)

// Event is a single keystroke. Timing is in microseconds so nothing is lost to
// truncation -- the fine structure (e.g. how long after a typo you pause before
// starting to correct it) survives.
//
// Reading the post-typo pause: it's the DTUS of the first Backspace event after
// a Typo. Any Correct/Typo events between the Typo and that Backspace are the
// "kept typing before noticing" run -- their count and summed DTUS say how far
// past the mistake you got before going back.
type Event struct {
	TUS      int64  `json:"t_us"`               // microseconds since the prompt started
	DTUS     int64  `json:"dt_us"`              // microseconds since the previous event
	Rune     string `json:"rune,omitempty"`     // the char, for insert/backspace/delete; empty for pure moves
	Kind     Kind   `json:"kind"`               // what the keystroke did
	Pos      int    `json:"pos"`                // cursor index when the key was pressed (before the action)
	Expected string `json:"expected,omitempty"` // for Typo: the character target has at this position
}

// Record is one completed prompt: the target, every keystroke, and the outcome.
type Record struct {
	Target        string  `json:"target"`
	StartedUnixMS int64   `json:"started_unix_ms"`
	DurationUS    int64   `json:"duration_us"`
	Final         string  `json:"final"`   // the buffer as submitted (after backspaces)
	Matched       bool    `json:"matched"` // Final == Target
	Events        []Event `json:"events"`
}

// Write appends r to w as a single JSON line.
func Write(w io.Writer, r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// Read parses a JSONL stream of Records.
func Read(r io.Reader) ([]Record, error) {
	var out []Record
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // long lines: many events
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}
