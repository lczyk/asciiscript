# asciiscript

Create [asciicasts](https://asciinema.org) without your fingers getting in the way.

Ever tried to record the perfect demo, but couldn't stop missing keys and having to restart?
`asciiscript` lets you record pre-scripted terminal sessions that look human.

It hosts `asciinema rec` inside a pty and injects your script keystroke-by-keystroke, so
asciinema records a real, interactive session with human-looking typing and real command output.
Targets asciinema 3.x (asciicast v3).

## Example

First, create a script.

```sh
echo "Hello, world..."
echo "Here's a demo of asciiscript."

# Comments with a '$' are control commands.

#$ delay 100  - Time between keypresses for subsequent commands (milliseconds).
echo "We can type slow..."
#$ delay 10
echo "Or quite fast."

#$ wait 100  - Pause after each command finishes, before the next (milliseconds).
sleep 1 && echo "The next line waits for this to finish on its own..."
#$ wait 500
echo "...and then this one gets a longer beat before it."

echo 'I hope you like it!'
```

Then record it.

```sh
$ asciiscript demo.sh demo.cast
$ asciinema play demo.cast
```

Three control commands:

- `#$ delay N` -- time between keypresses (typing speed), in ms. Default 40.
- `#$ wait N` -- pause after a command finishes, before the next one is typed, in ms.
  Default 100. The pause belongs to the line below it, so a `#$ wait` slows down the very
  next command. It's breathing room, not a runtime guess: waiting for the command itself is
  automatic (see [Waiting](#waiting)).
- `#$ handover` -- give the next command to whoever is running the recording. Takes no
  argument (see [Handover](#handover)).

Scripts run in a clean `bash` -- a minimal coloured prompt from a throwaway rcfile, no user
dotfiles, macOS deprecation banner silenced -- so demos come out consistent. Write the script
in bash syntax.

More scripts to record and crib from live in [`examples/`](examples), along with the
gotchas worth knowing before you write your own.

## Flags

```
    --cols     terminal width in columns (default: current terminal, else 80)
    --rows     terminal height in rows (default: current terminal, else 24)
    --settle   ms to wait for asciinema to warm up before typing (default 2000)
    --wait     ms to pause after each command finishes (default 100; #$ wait overrides)
    --speed    typing speed multiplier (default 1; 2 = twice as fast, scales #$ delay)
-q, --quiet    don't mirror the recorded session to this terminal
    --jitter   human-jitter scale (default 1; 0 = uniform/off, see below)
    --seed     rng seed for --jitter (default: random each run, printed on start)
    --cmd-timeout
               ms a command gets to finish before typing carries on (default 600000)
    --exit-timeout
               ms to wait for asciinema to stop once the script has typed exit
               (default 10000)
-v, --version  print the version and exit
```

```sh
$ asciiscript --cols 100 --rows 30 demo.sh demo.cast
```

## Waiting

Each command is typed only once the previous one has finished. `#$ wait` is the pause on
top of that, so a ten-minute build needs no `#$ wait 600000` -- the recording just takes
ten minutes, and asciinema's `idle_time_limit` compresses the dead air on playback.
Control lines cost nothing themselves; only typed commands get a pause.

This works by giving the recorded shell's prompt an invisible marker (an OSC 133 sequence,
carrying a token unique to the run) and watching for it. `PS2` carries it too, so heredocs
and lines continued with a trailing `\` wait like anything else.

The exception is a command that never returns to a prompt by itself -- an editor, a pager,
`ssh`, anything reading stdin. There's nothing to wait for: the keystrokes that would end it
are the ones being held back. Those run out `--cmd-timeout` (10 minutes by default), print a
warning naming the command, and get typed over anyway. Hand those over (below).

## Handover

`#$ handover` gives the next command to you. It's typed as usual, and then your keyboard is
wired to the recorded session until that command drops the shell back at a prompt -- at
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

Worth knowing: `--seed` no longer pins a take once a person is in it, and if `--cols`/`--rows`
don't match your actual terminal, the handed-over command draws itself to the recording's
size rather than yours. asciiscript warns when they differ.

[`examples/handover.sh`](examples/handover.sh) is a two-minute one to try.

## Jitter

By default the typing is jittered to look hand-done rather than machine-uniform. The model
(all of it in `jitter.go`, fitted to real captured typing) shapes only the timing -- it never
alters the text, so a recording always types the script exactly. Everything scales off the
base delay (`#$ delay`):

- **digraph-aware timing** -- each pause depends on the previous key: alternating hands are
  quick, same-finger reaches are slow, and there are longer pauses after spaces and punctuation.
  On top of that, per-key lognormal jitter.
- **hesitation** -- the occasional thinking stall mid-line.

`--jitter <scale>` sets the intensity: `1` (default) is the full human-like effect, values
below `1` ease it back toward uniform, and `0` is exactly uniform (steady `#$ delay` between
keys). Tune the model's constants in `jitter.go` to taste.

When jitter is on it's driven by a seeded rng, so a run is reproducible: the seed is random by
default and printed on start (`asciiscript: jitter 1 (seed 12345)`); pass `--seed 12345` to
replay a take you liked.

```sh
$ asciiscript demo.sh demo.cast                    # jittered (default), seed printed
$ asciiscript --jitter 0.5 demo.sh demo.cast       # subtler
$ asciiscript --jitter 0 demo.sh demo.cast         # uniform / machine-steady
$ asciiscript --seed 12345 demo.sh demo.cast       # reproduce a specific take
```

## Notes

- Requires the `asciinema` 3.x CLI on `PATH`. The output is asciicast v3.
- The output file is overwritten if it already exists.
- asciinema is run with `--quiet`, and its startup terminal queries (colour palette, cursor
  position, ...) are stripped from the live mirror -- otherwise the terminal's replies leak
  onto your shell prompt once recording ends.
- asciinema doesn't accept input for a second or two after launch, which is what `--settle`
  covers. Set it too low and every keystroke lands in the void, so the first line typed is
  checked for its echo and the run stops with an error rather than writing an empty recording.
- The script ends by typing `exit`. Anything still holding the terminal at that point -- a
  pager, an unterminated quote, a command reading stdin -- swallows it, so asciinema is given
  `--exit-timeout` to stop and is killed after that.
- `Ctrl-C` stops the recording rather than abandoning it: asciinema is told to stop so it can
  flush what it has, and the temporary rcfile is cleaned up.
- The `VERSION` file is the source of truth for the version. `make` stamps it into the binary
  along with the commit and build date, so `--version` reports what a build actually came
  from; a build that skips `make generate-version` reports `0.0.0-dev`.
