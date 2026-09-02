//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/creack/pty"
	flags "github.com/jessevdk/go-flags"
)

// options is the command-line configuration, parsed by go-flags.
type options struct {
	Cols   int     `long:"cols" description:"terminal width in columns (default 80, or the current terminal's when the script hands over)"`
	Rows   int     `long:"rows" description:"terminal height in rows (default 24, or the current terminal's when the script hands over)"`
	Speed  float64 `long:"speed" default:"1.0" description:"speed multiplier (2 = twice as fast; scales every #$ delay and #$ pause)"`
	Quiet  bool    `short:"q" long:"quiet" description:"do not echo the recorded session to this terminal"`
	Jitter float64 `long:"jitter" default:"1.0" description:"human-jitter scale (1 = human-like, 0 = uniform/off)"`
	Seed   *int64  `long:"seed" description:"rng seed for --jitter (default: random each run, printed on start)"`

	IdleTimeLimit float64 `long:"idle-time-limit" description:"cap on idle time between events in playback, in seconds, written into the recording (default: none)"`
	Title         string  `long:"title" description:"title written into the recording"`

	CmdTimeout  int `long:"cmd-timeout" default:"600000" description:"ms to wait for a command to finish before typing on regardless"`
	ExitTimeout int `long:"exit-timeout" default:"10000" description:"ms to wait for asciinema to stop once the session is ended"`

	Version bool `short:"v" long:"version" description:"print the version and exit"`

	Args struct {
		Script  string `positional-arg-name:"script" description:"script to type, or - for stdin"`
		Outfile string `positional-arg-name:"outfile" description:"output .cast file"`
	} `positional-args:"yes" required:"yes"`
}

// validate rejects flag values a recording can't be made with. It runs before
// anything else is looked at, so a bad flag is reported as such rather than
// masked by a missing script or a missing asciinema.
func (o *options) validate() error {
	switch {
	case o.Cols < 0 || o.Cols > 65535:
		return fmt.Errorf("--cols must be between 1 and 65535 (got %d)", o.Cols)
	case o.Rows < 0 || o.Rows > 65535:
		return fmt.Errorf("--rows must be between 1 and 65535 (got %d)", o.Rows)
	case o.Speed <= 0:
		return fmt.Errorf("--speed must be greater than 0 (got %g)", o.Speed)
	case o.Jitter < 0:
		return fmt.Errorf("--jitter must be >= 0 (got %g)", o.Jitter)
	case o.IdleTimeLimit < 0:
		return fmt.Errorf("--idle-time-limit must be >= 0 (got %g)", o.IdleTimeLimit)
	case o.CmdTimeout <= 0:
		return fmt.Errorf("--cmd-timeout must be greater than 0 (got %d)", o.CmdTimeout)
	case o.ExitTimeout <= 0:
		return fmt.Errorf("--exit-timeout must be greater than 0 (got %d)", o.ExitTimeout)
	}
	return nil
}

func main() {
	log.SetFlags(0)

	// Errors are printed here rather than by go-flags: the struct is filled in
	// by the time the missing positionals are reported, so --version can be
	// honoured on its own without that complaint going out first.
	var opts options
	parser := flags.NewParser(&opts, flags.HelpFlag|flags.PassDoubleDash)
	parser.Usage = "[OPTIONS]"
	_, err := parser.Parse()
	if opts.Version {
		fmt.Println(versionLine())
		os.Exit(0)
	}
	if err != nil {
		if flags.WroteHelp(err) {
			fmt.Println(err)
			os.Exit(0)
		}
		log.Fatal(err)
	}

	if err := opts.validate(); err != nil {
		log.Fatal(err)
	}
	if err := checkAsciinema(); err != nil {
		log.Fatal(err)
	}

	s, err := loadScriptOrStdin(opts.Args.Script)
	if err != nil {
		log.Fatal("parsing script failed: ", err)
	}

	if err := record(s, &opts); err != nil {
		log.Fatal(err)
	}
}

// loadScriptOrStdin is loadScript, with `-` for standard input.
func loadScriptOrStdin(path string) (*script, error) {
	if path != "-" {
		return loadScript(path)
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, err
	}
	return parseScript(string(b))
}

// checkAsciinema makes sure the asciinema on PATH is the 3.x generation the
// recording is written against. The 2.x CLI takes the same flags, so a
// mismatch would otherwise surface halfway through a take.
func checkAsciinema() error {
	if _, err := exec.LookPath("asciinema"); err != nil {
		return errors.New("can't find asciinema executable on PATH")
	}
	out, err := exec.Command("asciinema", "--version").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return fmt.Errorf("asciinema --version failed: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return fmt.Errorf("asciinema --version failed: %w", err)
	}
	major, ok := asciinemaMajor(string(out))
	switch {
	case !ok:
		log.Printf("asciiscript: couldn't tell asciinema's version from %q -- carrying on, 3.x is needed", strings.TrimSpace(string(out)))
	case major < 3:
		return fmt.Errorf("asciinema 3.x is needed, but `asciinema --version` says %q", strings.TrimSpace(string(out)))
	}
	return nil
}

// asciinemaMajor reads the major version out of `asciinema --version`'s
// output, which is "asciinema 3.2.1" or thereabouts.
func asciinemaMajor(out string) (int, bool) {
	for _, field := range strings.Fields(out) {
		major, _, _ := strings.Cut(field, ".")
		if n, err := strconv.Atoi(major); err == nil && n >= 0 {
			return n, true
		}
	}
	return 0, false
}

// termSize resolves the recording window size: what the command line says,
// else 80x24, or the current terminal's size with fromTerminal (a handed-over
// command draws itself to the recording's size, so that had better be the screen).
func termSize(o *options, fromTerminal bool) (cols, rows uint16) {
	cols, rows = uint16(o.Cols), uint16(o.Rows)
	dc, dr := uint16(80), uint16(24)
	if fromTerminal {
		if ws, err := pty.GetsizeFull(os.Stdin); err == nil && ws.Cols > 0 && ws.Rows > 0 {
			dc, dr = ws.Cols, ws.Rows
		}
	}
	if cols == 0 {
		cols = dc
	}
	if rows == 0 {
		rows = dr
	}
	return cols, rows
}
