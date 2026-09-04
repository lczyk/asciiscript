# asciiscript

Create [asciicasts](https://asciinema.org) without your fingers getting in the way.

Ever tried to record the perfect demo, but couldn't stop missing keys and having to restart?
`asciiscript` lets you record pre-scripted terminal sessions that look human.

It runs `bash` inside a pty, injects your script keystroke-by-keystroke, and records what comes
back: a real, interactive session with human-looking typing and real command output, written as
an [asciicast v3](https://docs.asciinema.org/manual/asciicast/v3/) for asciinema to play.

## Install

Needs Go and `bash`; the [asciinema](https://asciinema.org) CLI (3.x) to play what you record.
macOS and Linux; it doesn't run on Windows, since the recording is hosted in a Unix pty.

```sh
$ go install github.com/lczyk/asciiscript@latest
```

Or from a checkout, `make build` puts the binary in `./bin` and `make install` symlinks it into
`~/.local/bin`.

## Example

First, create a script.

```sh
echo "Hello, world..."
echo "Here's a demo of asciiscript."

# Comments with a '$' are control lines. Each one applies to the next command only.

#$ delay 100  - Time between keypresses for this command (milliseconds).
echo "We can type slow..."
#$ delay 10
echo "Or quite fast."

echo "And the line after is back to the usual pace."

#$ pause 1500  - Hold the previous output on screen this long before typing (milliseconds).
echo "...that gave the line above room to be read."

sleep 1 && echo "A slow command needs no pause: the next line waits for it on its own."
echo 'I hope you like it!'
#$ pause 1000
```

Then record it.

```sh
$ asciiscript demo.sh demo.cast
$ asciinema play demo.cast
```

Three control lines, each for the one command written under it:

- `#$ delay N` -- time between keypresses (typing speed) for that command, in ms. Default 40.
- `#$ pause N` -- how long to sit at the prompt before typing that command, in ms. By default
  the gap is a beat of the typing model's own (a couple of keystrokes' worth), which is enough
  for the eye to follow; a `#$ pause` is for letting output be read. It's breathing room, not
  a runtime guess: waiting for the previous command to finish is automatic (see
  [Waiting](#waiting)). At the very end of a script, a `#$ pause` holds the last prompt that
  long before the session ends.
- `#$ handover` -- give that command to whoever is running the recording. Takes no argument
  (see [Handover](#handover)).

Both numbers must be between 0 and an hour.

A command is usually one line. A heredoc, a line ending in `\`, or a quote left open runs it on
to the following lines, and asciiscript reads those the way bash does: as part of the same
command, every line literal -- blank lines and `#$` lines included -- and any control lines in
front apply to the whole of it. A command that never ends (a heredoc missing its terminator, a
quote never closed) is a parse error, as is a `#$ delay` or `#$ handover` with nothing after it.

Other multi-line constructs -- `if`/`for`/`while` blocks, `{ }`, `( )`, a line ending in `|` or
`&&` -- are typed line by line, each waited for at the shell's continuation prompt like any
other. That works, but every physical line is its own command to asciiscript: blank lines inside
the block are skipped, and a control line inside it applies to the next line of the block.

Scripts run in a clean `bash` -- a minimal coloured prompt from a throwaway rcfile, no user
dotfiles, no readline, macOS deprecation banner silenced -- so demos come out consistent. The prompt shows
the working directory the way fish's `prompt_pwd` does (`~/g/asciiscript`), so a deep path
doesn't wrap it, and it's read-only: `PS1` is how asciiscript follows the session along (see
[Waiting](#waiting)), so a script can't set its own. Everything else in the environment --
`TERM`, `LANG`, `LC_*`, `PATH` -- is inherited as is. Write the script in bash syntax.

More scripts to record and crib from live in [`examples/`](examples), along with the
gotchas worth knowing before you write your own.

## Flags

```
    --cols     terminal width in columns (default 80, or the current terminal's
               when the script hands over)
    --rows     terminal height in rows (default 24, likewise)
    --speed    take speed multiplier (default 1; 2 = twice as fast, scales every
               #$ delay and #$ pause and the model's own gaps)
-q, --quiet    don't mirror the recorded session to this terminal
    --jitter   human-jitter scale (default 1; 0 = uniform/off, see below)
    --seed     rng seed for --jitter (default: random each run, printed on start)
    --idle-time-limit
               cap on idle time between events in playback, in seconds, written
               into the recording (default: none)
    --title    title written into the recording
    --capture-input
               record the keystrokes the script types as input events (a
               handover's never are)
    --cmd-timeout
               ms a command gets to finish before typing carries on (default 600000)
    --exit-timeout
               ms to wait for the shell to exit once the session is ended
               (default 10000)
-v, --version  print the version and exit
```

```sh
$ asciiscript --cols 100 --rows 30 demo.sh demo.cast
$ asciiscript --title "Installing it" --idle-time-limit 2 demo.sh demo.cast
```

The script can also come from standard input, as `-`.

The recording is 80x24 unless told otherwise, whatever terminal it's made from: a recording is
for other people's screens. Nothing in it depends on that size, though: the recorded shell
runs without readline, so what's typed is echoed by the terminal itself and a long line wraps
wherever the recording is played, not where it was made.

## Waiting

Each command is typed only once the previous one has finished. `#$ pause` is the beat on
top of that, so a ten-minute build needs no `#$ pause 600000` -- the recording just takes
ten minutes. Pass `--idle-time-limit 2` to have players compress the dead air to two seconds
(it's a field in the recording, not a change to it). Control lines cost nothing themselves;
only typed commands get a pause.

A `#$ pause` in front of a command also leaves a marker in the recording, named after that
command and placed where the typing resumes: `asciinema play -m` pauses at each one, and `]`
skips to the next. The pause at the very end of a script has no command to name one after, so
it gets none.

This works by giving the recorded shell's prompt an invisible marker (an OSC 133 sequence,
carrying a token unique to the run) and watching for it. `PS2` carries it too, so the lines
of a heredoc or a `\`-continued command are each waited for like anything else. The first
prompt is what starts the typing: it's the sign that bash has loaded its rcfile and is
listening.

The exception is a command that never returns to a prompt by itself -- an editor, a pager,
`ssh`, anything reading stdin. There's nothing to wait for: the keystrokes that would end it
are the ones being held back. Those run out `--cmd-timeout` (10 minutes by default), print a
warning naming the command, and get typed over anyway. Hand those over (below).

## Handover

`#$ handover` gives the command under it to you. It's typed as usual, and then your keyboard
is wired to the recorded session until that command drops the shell back at a prompt -- at
which point the script picks up again on its own. What you did is in the recording, typed by
hand and indistinguishable from the rest.

```sh
#$ handover
nano config.yaml
```

That's the answer for the commands nothing can drive for you: editors, `ssh`, a REPL,
anything waiting on a keypress. There's no deadline on a handover -- `--cmd-timeout` doesn't
apply, because it's waiting on a person.

Your terminal goes into raw mode for the duration, so ctrl-o, ctrl-c, arrows and the rest
reach the command rather than being buffered into lines or turned into signals. Which also
means ctrl-c won't stop asciiscript while a handover is live: quit the command first.

Two things a handover needs, both checked before the take starts:

- not `--quiet` -- you can't drive what you can't see
- a real terminal on stdin

Worth knowing: `--seed` no longer pins a take once a person is in it. A script with a
handover in it is recorded at your terminal's size rather than 80x24, since the handed-over
command draws itself to the recording's size and you'll be looking at it through yours; if
`--cols`/`--rows` say otherwise, asciiscript warns. Resizing the terminal during a handover
resizes the recording with it, and the recording says so, so playback follows.

[`examples/handover.sh`](examples/handover.sh) is a two-minute one to try.

## Jitter

By default the typing is jittered to look hand-done rather than machine-uniform. The model
(all of it in `jitter.go`, fitted to real captured typing) shapes only the timing -- it never
alters the text, so a recording always types the script exactly. Everything scales off the
per-keystroke delay (`#$ delay`):

- **digraph-aware timing** -- each pause depends on the previous key: alternating hands are
  quick, same-finger reaches are slow, and there are longer pauses after spaces, punctuation
  and at the start of a line. On top of that, per-key lognormal jitter.
- **hesitation** -- the occasional thinking stall mid-line.

A `#$ pause` is jittered too, around the value asked for, so a scripted beat doesn't land
with machine precision either.

`--jitter <scale>` sets the intensity: `1` (default) is the full human-like effect, values
below `1` ease it back toward uniform, and `0` is exactly uniform (a steady `#$ delay` between
keys, a `#$ pause` to the millisecond). Tune the model's constants in `jitter.go` to taste.

When jitter is on it's driven by a seeded rng, so a run is reproducible: the seed is random by
default and printed on start (`asciiscript: jitter 1 (seed 4821)`); pass `--seed 4821` to
replay a take you liked. The typing is then the same to the keystroke; what the commands
print still lands when it lands, so two takes differ by a millisecond here and there.

```sh
$ asciiscript demo.sh demo.cast                    # jittered (default), seed printed
$ asciiscript --jitter 0.5 demo.sh demo.cast       # subtler
$ asciiscript --jitter 0 demo.sh demo.cast         # uniform / machine-steady
$ asciiscript --seed 4821 demo.sh demo.cast        # reproduce a specific take
```

## Notes

- The output is asciicast v3, written by asciiscript itself; nothing but `bash` is needed to
  record. The header carries the size, the time, `--title` and `--idle-time-limit`, and
  `SHELL` as the bash that ran; a `#$ pause` before a command is an `m` event, a handover
  resize an `r` event, and the last event is `x` with the shell's exit status.
- The output file is overwritten if it already exists.
- The recorded session's terminal queries (colour palette, cursor position, ...) are stripped
  from the live mirror -- otherwise the terminal's replies leak onto your shell prompt once
  recording ends. During a handover they go through, since you're there to have them
  answered. The recording has them as they were.
- The session is ended the way you'd end one by hand: end-of-input (ctrl-d) at the prompt,
  which bash answers with its own `exit`. Anything still holding the terminal at that point --
  a pager, a command reading stdin -- swallows it, so the shell is given `--exit-timeout` to
  exit and is killed after that.
- `Ctrl-C` (or the terminal window closing) stops the recording rather than abandoning it:
  the shell is ended, what it printed is written out, and the temporary rcfile is cleaned up.
- A script is typed byte for byte, but the session's *output* is written as UTF-8, so anything
  that isn't comes out as U+FFFD in the recording -- as asciinema would have it.
- `--version` reports the version the binary was built from: the git tag for a build at one
  (a `go install` of a tag included), a pseudo-version with the commit otherwise, `+dirty`
  when the tree had uncommitted changes, and the `VERSION` file's number where there is no
  git at all.

asciiscript started as a fork of [christopher-dG/asciiscript](https://github.com/christopher-dG/asciiscript)
and has since been rewritten; the [changelog](CHANGELOG.md) has the history from there.
