package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"

	"github.com/creack/pty"
	flags "github.com/jessevdk/go-flags"
)

// options is the command-line configuration, parsed by go-flags.
type options struct {
	Cols   int     `long:"cols" description:"terminal width in columns (default: current terminal)"`
	Rows   int     `long:"rows" description:"terminal height in rows (default: current terminal)"`
	Settle int     `long:"settle" default:"2000" description:"ms to wait for asciinema to warm up before typing"`
	Speed  float64 `long:"speed" default:"1.0" description:"typing speed multiplier (2 = twice as fast; scales #$ delay)"`
	Quiet  bool    `short:"q" long:"quiet" description:"do not echo the recorded session to this terminal"`
	Jitter float64 `long:"jitter" default:"1.0" description:"human-jitter scale (1 = human-like, 0 = uniform/off)"`
	Seed   *int64  `long:"seed" description:"rng seed for --jitter (default: random each run, printed on start)"`

	CmdTimeout  int `long:"cmd-timeout" default:"600000" description:"ms to wait for a command to finish before typing on regardless"`
	ExitTimeout int `long:"exit-timeout" default:"10000" description:"ms to wait for asciinema to stop once the script has typed exit"`

	// Handled before parsing, since --version has no business needing the
	// positional args; declared here only so it shows up in --help.
	Version bool `short:"v" long:"version" description:"print the version and exit"`

	Args struct {
		Script  string `positional-arg-name:"script" description:"script to type"`
		Outfile string `positional-arg-name:"outfile" description:"output .cast file"`
	} `positional-args:"yes" required:"yes"`
}

func main() {
	log.SetFlags(0)

	if wantsVersion(os.Args[1:]) {
		fmt.Println(versionLine())
		os.Exit(0)
	}

	var opts options
	parser := flags.NewParser(&opts, flags.Default)
	parser.Usage = "[OPTIONS]"
	if _, err := parser.Parse(); err != nil {
		if flags.WroteHelp(err) {
			os.Exit(0)
		}
		os.Exit(1) // go-flags already printed the error
	}

	if _, err := exec.LookPath("asciinema"); err != nil {
		log.Fatal("can't find asciinema executable on PATH")
	}

	s, err := loadScript(opts.Args.Script)
	if err != nil {
		log.Fatal("parsing script failed: ", err)
	}

	if err := s.record(&opts); err != nil {
		log.Fatal(err)
	}
}

// termSize resolves the recording window size. A dimension set on the command
// line wins; any left at 0 is filled from the current terminal (stdin), falling
// back to 80x24 when stdin isn't a terminal (e.g. piped or backgrounded).
func termSize(o *options) (cols, rows uint16) {
	cols, rows = uint16(o.Cols), uint16(o.Rows)
	if cols != 0 && rows != 0 {
		return cols, rows
	}
	dc, dr := uint16(80), uint16(24)
	if ws, err := pty.GetsizeFull(os.Stdin); err == nil && ws.Cols > 0 && ws.Rows > 0 {
		dc, dr = ws.Cols, ws.Rows
	}
	if cols == 0 {
		cols = dc
	}
	if rows == 0 {
		rows = dr
	}
	return cols, rows
}
