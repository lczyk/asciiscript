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

#$ wait 100  - Time between commands for subsequent commands (milliseconds).
sleep 1 && echo "We can wait for output..."
#$ wait 500
echo "Because otherwise, things could get a bit weird."

echo "I hope you like it!"
```

Then record it.

```sh
$ asciiscript demo.sh demo.cast
$ asciinema play demo.cast
```

Two control commands, both in milliseconds:

- `#$ delay N` -- time between keypresses (typing speed). Default 40.
- `#$ wait N` -- time between commands. Default 100. Set this comfortably above a
  command's runtime, or the next input gets typed while it's still running.

Scripts run in a clean `bash` -- a minimal coloured prompt from a throwaway rcfile, no user
dotfiles, macOS deprecation banner silenced -- so demos come out consistent. Write the script
in bash syntax.

## Flags

```
    --cols     terminal width in columns (default: current terminal, else 80)
    --rows     terminal height in rows (default: current terminal, else 24)
    --settle   ms to wait for asciinema to warm up before typing (default 2000)
    --wait     ms to sleep between commands (default 100; #$ wait overrides)
    --speed    typing speed multiplier (default 1; 2 = twice as fast, scales #$ delay)
-q, --quiet    don't mirror the recorded session to this terminal
    --human    type like a human (see below)
    --seed     rng seed for --human (default: random each run, printed on start)
```

```sh
$ asciiscript --cols 100 --rows 30 demo.sh demo.cast
```

## Human typing

By default keystrokes land at a steady interval (the `#$ delay`), which reads as
machine-typed. `--human` swaps in a timing model (all of it in `human.go`, fitted to real
captured typing) that makes the typing look hand-done. It never fabricates mistakes, so the
typed text always matches the script. Everything scales off the base delay:

- **digraph-aware timing** -- each pause depends on the previous key: alternating hands are
  quick, same-finger reaches are slow, and there are longer pauses after spaces and punctuation.
  On top of that, per-key lognormal jitter.
- **hesitation** -- the occasional thinking stall mid-line.

Tune the constants at the top of `human.go` to taste.

It's driven by a seeded rng, so a run is reproducible: the seed is random by default and printed
on start (`asciiscript: human typing (seed 12345)`); pass `--seed 12345` to replay a take you liked.

```sh
$ asciiscript --human demo.sh demo.cast          # fresh randomness, seed printed
$ asciiscript --human --seed 12345 demo.sh demo.cast   # reproduce a specific take
```

## Notes

- Requires the `asciinema` 3.x CLI on `PATH`. The output is asciicast v3.
- The output file is overwritten if it already exists.
- asciinema is run with `--quiet`, and its startup terminal queries (colour palette, cursor
  position, ...) are stripped from the live mirror -- otherwise the terminal's replies leak
  onto your shell prompt once recording ends.
