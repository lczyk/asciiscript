package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
)

// The unit tests drive Script against a fake pty and a fake clock, which leaves
// the part that only exists in a real terminal -- asciinema, the pty, the bash
// rcfile and the prompt marker that ties them together -- unexercised. These
// tests record for real and read the .cast back.

// event is one asciicast v3 event. Times in the file are the gap since the
// previous event; at is that accumulated into seconds from the start.
type event struct {
	gap  float64
	at   float64
	kind string
	data string
}

// record runs a script through a real `asciinema rec` and returns the events it
// wrote. Skipped rather than failed where asciinema isn't installed: the rest of
// the suite has no such dependency and shouldn't grow one.
func record(t *testing.T, script string, o Options) []event {
	t.Helper()
	if testing.Short() {
		t.Skip("records for real, which takes seconds")
	}
	if _, err := exec.LookPath("asciinema"); err != nil {
		t.Skip("asciinema isn't on PATH")
	}

	out := filepath.Join(t.TempDir(), "out.cast")
	o.Args.Script, o.Args.Outfile = "", out
	o.Quiet = true // don't spray the recorded session over the test output
	o.Jitter = 0   // uniform typing: nothing here is testing the timing model
	if o.Settle == 0 {
		o.Settle = 2000
	}
	if o.Timeout == 0 {
		o.Timeout = 10000
	}
	if o.CmdSync == 0 {
		o.CmdSync = 600000
	}
	if o.Speed == 0 {
		o.Speed = 2 // twice as fast: less wall clock, same behaviour
	}

	s, err := parseScript(script)
	if err != nil {
		t.Fatalf("parsing the script failed: %v", err)
	}
	if err := s.Run(&o); err != nil {
		t.Fatalf("recording failed: %v", err)
	}
	return readCast(t, out)
}

func readCast(t *testing.T, path string) []event {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no recording was written: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 {
		t.Fatalf("a recording should have a header and some events, got %q", string(b))
	}

	var header struct {
		Version int `json:"version"`
	}
	assert.NoError(t, json.Unmarshal([]byte(lines[0]), &header))
	assert.Equal(t, header.Version, 3, "asciiscript targets asciicast v3")

	var events []event
	var at float64
	for _, line := range lines[1:] {
		var raw []any
		assert.NoError(t, json.Unmarshal([]byte(line), &raw), line)
		assert.Len(t, raw, 3, line)
		gap := raw[0].(float64)
		at += gap
		events = append(events, event{gap: gap, at: at, kind: raw[1].(string), data: raw[2].(string)})
	}
	return events
}

// output is everything the recorded session printed, keystroke echoes included,
// as one string -- which is how a viewer sees it.
func output(events []event) string {
	var b strings.Builder
	for _, e := range events {
		if e.kind == "o" {
			b.WriteString(e.data)
		}
	}
	return b.String()
}

// submitted returns the index of the event at which line had been fully typed
// and entered. Keystrokes arrive one event at a time, so this is the first
// event by which the accumulated output contains the whole echoed line.
func submitted(t *testing.T, events []event, line string) int {
	t.Helper()
	var b strings.Builder
	for i, e := range events {
		if e.kind != "o" {
			continue
		}
		b.WriteString(e.data)
		if strings.Contains(b.String(), line+"\r\n") {
			return i
		}
	}
	t.Fatalf("never saw %q submitted in the recording", line)
	return 0
}

func TestRecordsASession(t *testing.T) {
	events := record(t, "#$ wait 50\necho alpha\necho bravo\n", Options{})

	got := output(events)
	assert.ContainsString(t, got, "alpha\r\n")
	assert.ContainsString(t, got, "bravo\r\n")
	assert.That(t, strings.Index(got, "alpha\r\n") < strings.Index(got, "bravo\r\n"),
		"the script's order should be the recording's order")

	last := events[len(events)-1]
	assert.Equal(t, last.kind, "x", "the recording should end with an exit event")
	assert.Equal(t, last.data, "0", "the recorded shell should exit cleanly")
}

// The one that matters: `#$ wait 50` is nowhere near the two seconds `sleep`
// takes, so the only thing that can stop `echo bravo` being typed into the
// running sleep is the wait for its prompt. When that regresses, the keystrokes
// show up in the recording a few hundred milliseconds after the sleep starts.
func TestWaitsForASlowCommandBeforeTypingTheNext(t *testing.T) {
	events := record(t, "#$ wait 50\necho alpha\nsleep 2\necho bravo\n", Options{})

	i := submitted(t, events, "sleep 2")
	next := events[i+1]
	assert.That(t, next.gap >= 2,
		"nothing should reach the terminal for the 2s `sleep 2` runs, but the next event came after "+
			formatSeconds(next.gap))

	assert.ContainsString(t, output(events), "bravo\r\n")
}

// And the escape hatch really is an escape hatch: with --no-sync the same
// script goes back to typing over the running command.
func TestNoSyncTypesOverARunningCommand(t *testing.T) {
	events := record(t, "#$ wait 50\necho alpha\nsleep 2\necho bravo\n", Options{NoSync: true})

	i := submitted(t, events, "sleep 2")
	next := events[i+1]
	assert.That(t, next.gap < 2,
		"--no-sync should type on regardless, but the next event waited "+formatSeconds(next.gap))
}

// A marker in every prompt is what the wait watches for, so it has to be in the
// recording -- and --no-sync, which needs no marker, shouldn't leave one.
func TestPromptsCarryTheMarker(t *testing.T) {
	assert.ContainsString(t, output(record(t, "echo hi\n", Options{})), "\x1b]133;D;")

	bare := output(record(t, "echo hi\n", Options{NoSync: true}))
	assert.That(t, !strings.Contains(bare, "\x1b]133;D;"),
		"--no-sync should leave the recording unmarked")
}

// Continuation lines sit at PS2, which carries the marker too, so a heredoc
// shouldn't spend a --cmd-timeout per line. The timeout here is short enough
// that waiting one out would be unmistakable.
func TestHeredocDoesNotStallOnEveryLine(t *testing.T) {
	events := record(t,
		"#$ wait 50\ncat <<'YAML'\nname: demo\nbase: bare\nYAML\n",
		Options{CmdSync: 3000})

	got := output(events)
	assert.ContainsString(t, got, "name: demo\r\nbase: bare\r\n")

	total := events[len(events)-1].at
	assert.That(t, total < 3,
		"the heredoc should flow, not wait out a timeout per line; took "+formatSeconds(total))
}

func formatSeconds(f float64) string {
	b, _ := json.Marshal(f)
	return string(b) + "s"
}
