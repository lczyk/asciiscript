//go:build !windows

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

// The unit tests drive script against a fake pty and a fake clock, which leaves
// the part that only exists in a real terminal -- the pty, bash, the rcfile and
// the prompt marker that ties them together -- unexercised. These tests record
// for real and read the .cast back.

// event is one asciicast v3 event. Times in the file are the gap since the
// previous event; at is that accumulated into seconds from the start.
type event struct {
	gap  float64
	at   float64
	kind string
	data string
}

// capture records a script for real -- a bash in a pty -- and returns the
// events written.
func capture(t *testing.T, script string, o options) []event {
	t.Helper()
	if testing.Short() {
		t.Skip("records for real, which takes seconds")
	}

	out := filepath.Join(t.TempDir(), "out.cast")
	o.Args.Script, o.Args.Outfile = "demo.sh", out
	o.Quiet = true // don't spray the recorded session over the test output
	o.Jitter = 0   // uniform typing: nothing here is testing the timing model
	if o.ExitTimeout == 0 {
		o.ExitTimeout = 10000
	}
	if o.CmdTimeout == 0 {
		o.CmdTimeout = 600000
	}
	if o.Speed == 0 {
		o.Speed = 2 // twice as fast: less wall clock, same behaviour
	}

	s, err := parseScript(script)
	if err != nil {
		t.Fatalf("parsing the script failed: %v", err)
	}
	if err := record(s, &o); err != nil {
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
	events := capture(t, "echo alpha\necho bravo\n", options{})

	got := output(events)
	assert.ContainsString(t, got, "alpha\r\n")
	assert.ContainsString(t, got, "bravo\r\n")
	assert.That(t, strings.Index(got, "alpha\r\n") < strings.Index(got, "bravo\r\n"),
		"the script's order should be the recording's order")

	last := events[len(events)-1]
	assert.Equal(t, last.kind, "x", "the recording should end with an exit event")
	assert.Equal(t, last.data, "0", "the recorded shell should exit cleanly")
}

// The one that matters: the model's line gap is nowhere near the two seconds
// `sleep` takes, so the only thing that can stop `echo bravo` being typed into
// the running sleep is the wait for its prompt. When that regresses, the keystrokes
// show up in the recording a few hundred milliseconds after the sleep starts.
func TestWaitsForASlowCommandBeforeTypingTheNext(t *testing.T) {
	events := capture(t, "echo alpha\nsleep 2\necho bravo\n", options{})

	i := submitted(t, events, "sleep 2")
	next := events[i+1]
	assert.That(t, next.gap >= 2,
		"nothing should reach the terminal for the 2s `sleep 2` runs, but the next event came after "+
			formatSeconds(next.gap))

	assert.ContainsString(t, output(events), "bravo\r\n")
}

// A marker in every prompt is what the wait watches for, so it has to be in the
// recording.
func TestPromptsCarryTheMarker(t *testing.T) {
	assert.ContainsString(t, output(capture(t, "echo hi\n", options{})), "\x1b]133;D;")
}

// Continuation lines sit at PS2, which carries the marker too, so a heredoc
// shouldn't spend a --cmd-timeout per line. The timeout here is short enough
// that waiting one out would be unmistakable.
func TestHeredocDoesNotStallOnEveryLine(t *testing.T) {
	events := capture(t,
		"cat <<'YAML'\nname: demo\nbase: bare\nYAML\n",
		options{CmdTimeout: 3000})

	got := output(events)
	assert.ContainsString(t, got, "name: demo\r\nbase: bare\r\n")

	total := events[len(events)-1].at
	assert.That(t, total < 3,
		"the heredoc should flow, not wait out a timeout per line; took "+formatSeconds(total))
}

// A backslash continuation is one command to bash and to asciiscript alike:
// typed line by line at PS2, run once, waited for once.
func TestBackslashContinuationRuns(t *testing.T) {
	events := capture(t, "echo one \\\n  two\n", options{CmdTimeout: 3000})
	assert.ContainsString(t, output(events), "one two\r\n")
	assert.That(t, events[len(events)-1].at < 3, "should not have waited out a timeout")
}

// A trailing pause is the one control line with nothing after it: it holds
// the last prompt before the session ends, and the recording is that much
// longer for it.
func TestTrailingPauseHoldsTheLastPrompt(t *testing.T) {
	quick := capture(t, "echo hi\n", options{})
	held := capture(t, "echo hi\n#$ pause 1500\n", options{})
	// --speed 2 in record halves the pause asked for.
	assert.That(t, held[len(held)-1].at-quick[len(quick)-1].at > 0.5,
		"the held recording should run longer by about the pause")
}

func formatSeconds(f float64) string {
	b, _ := json.Marshal(f)
	return string(b) + "s"
}

// readHeader is the recording's first line.
func readHeader(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	assert.NoError(t, err)
	line, _, _ := strings.Cut(string(b), "\n")
	var header map[string]any
	assert.NoError(t, json.Unmarshal([]byte(line), &header))
	return header
}

// The last event is how the shell exited: end-of-input at the prompt exits
// with the last command's status.
func TestExitStatusIsRecorded(t *testing.T) {
	events := capture(t, "false\n", options{})
	last := events[len(events)-1]
	assert.Equal(t, last.kind, "x")
	assert.Equal(t, last.data, "1")
}

// A `#$ pause` is a marker in the recording, after the paused-for output and
// before the paused command is typed.
func TestPauseBecomesAMarker(t *testing.T) {
	events := capture(t, "echo alpha\n#$ pause 300\necho bravo\n", options{})
	m := -1
	for i, e := range events {
		if e.kind == "m" {
			assert.Equal(t, m, -1, "only one command was paused")
			m = i
		}
	}
	assert.That(t, m > 0, "the pause should have left a marker")
	assert.Equal(t, events[m].data, "echo bravo")
	assert.ContainsString(t, output(events[:m]), "alpha\r\n")
	assert.That(t, !strings.Contains(output(events[:m]), "echo bravo"), "the marker comes before the paused command is typed")
	assert.ContainsString(t, output(events[m:]), "bravo\r\n")
}

func TestHeaderDescribesTheTake(t *testing.T) {
	if testing.Short() {
		t.Skip("records for real")
	}
	out := filepath.Join(t.TempDir(), "out.cast")
	sc, err := parseScript("echo hi\n")
	assert.NoError(t, err)
	assert.NoError(t, record(sc, &options{
		Quiet: true, Jitter: 0, Speed: 4, CmdTimeout: 600000, ExitTimeout: 10000,
		Title: "a take", IdleTimeLimit: 1.5,
		Args: struct {
			Script  string `positional-arg-name:"script" description:"script to type, or - for stdin"`
			Outfile string `positional-arg-name:"outfile" description:"output .cast file"`
		}{Script: "examples/demo.sh", Outfile: out},
	}))

	h := readHeader(t, out)
	assert.Equal(t, h["version"].(float64), 3)
	term := h["term"].(map[string]any)
	assert.Equal(t, term["cols"].(float64), 80)
	assert.Equal(t, term["rows"].(float64), 24)
	assert.Equal(t, h["title"].(string), "a take")
	assert.Equal(t, h["idle_time_limit"].(float64), 1.5)
	assert.Equal(t, h["command"].(string), "asciiscript demo.sh")
	assert.That(t, h["timestamp"].(float64) > 0, "the take is dated")
	assert.That(t, strings.HasSuffix(h["env"].(map[string]any)["SHELL"].(string), "bash"), "the shell is the bash that ran")
}

// The keystrokes the script types can be recorded as input events; a viewer
// then sees what was typed as well as what came back.
func TestCaptureInputRecordsTheKeystrokes(t *testing.T) {
	events := capture(t, "echo hi\n", options{CaptureInput: true})
	var typed strings.Builder
	for _, e := range events {
		if e.kind == "i" {
			typed.WriteString(e.data)
		}
	}
	assert.Equal(t, typed.String(), "echo hi\n")
}

// asciinema is the player the recording is for; where it's installed, it has
// to accept what was written.
func TestAsciinemaReadsTheRecording(t *testing.T) {
	if testing.Short() {
		t.Skip("records for real")
	}
	if _, err := exec.LookPath("asciinema"); err != nil {
		t.Skip("asciinema isn't on PATH")
	}
	out := filepath.Join(t.TempDir(), "out.cast")
	sc, err := parseScript("echo alpha\n#$ pause 200\necho 'b<r>&avo'\n")
	assert.NoError(t, err)
	assert.NoError(t, record(sc, &options{
		Quiet: true, Jitter: 0, Speed: 4, CmdTimeout: 600000, ExitTimeout: 10000, CaptureInput: true,
		Args: struct {
			Script  string `positional-arg-name:"script" description:"script to type, or - for stdin"`
			Outfile string `positional-arg-name:"outfile" description:"output .cast file"`
		}{Script: "demo.sh", Outfile: out},
	}))

	txt, err := exec.Command("asciinema", "convert", "-q", "-f", "txt", out, "-").Output()
	assert.NoError(t, err, "asciinema convert should accept the recording")
	assert.ContainsString(t, string(txt), "alpha")
	assert.ContainsString(t, string(txt), "b<r>&avo")

	again := filepath.Join(t.TempDir(), "again.cast")
	_, err = exec.Command("asciinema", "convert", "-q", "--overwrite", out, again).Output()
	assert.NoError(t, err, "asciinema should round-trip the recording")
	assert.Equal(t, output(readCast(t, again)), output(readCast(t, out)))
}
