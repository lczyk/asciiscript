# Examples

Ready-to-record scripts. Record any of them the usual way:

```sh
$ asciiscript examples/hello.sh hello.cast
$ asciinema play hello.cast
```

- **`hello.sh`** -- the README demo. The smallest thing that shows `#$ delay` and `#$ pause`.
- **`pacing.sh`** -- `#$ delay` and `#$ pause` in anger: fast lines, slow lines, and giving
  output room to breathe.
- **`handover.sh`** -- `#$ handover` in practice: the script sets a file up, hands you the
  editor to change one line, and carries on once you quit. Needs a real terminal, so run it
  yourself rather than from a pipe -- and `nano`, which ships on macOS and most desktop
  Linux but not on minimal images; swap in whatever editor you have.
- **`git.sh`** -- a full workflow (init, edit, commit, log) run in a scratch directory under
  `/tmp` that the script clears out first, so takes are repeatable. Its one `export` line is
  doing real work: `GIT_PAGER=cat` stops `git log` and `git diff` opening `less` and hanging,
  and pointing `GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM` at `/dev/null` keeps your own git
  config out of the recording -- `commit.gpgsign = true` alone is enough to break the demo on
  someone else's machine.

A few things worth trying on any of them:

```sh
$ asciiscript --jitter 0 examples/pacing.sh out.cast     # machine-steady typing
$ asciiscript --speed 2 examples/pacing.sh out.cast      # same script, twice as fast
$ asciiscript --cols 100 --rows 30 examples/git.sh out.cast
$ asciiscript --seed 4821 examples/git.sh out.cast       # reproduce a take you liked
```

## Writing your own

- **Only `#$` lines are control lines.** Every other line is typed into the shell, ordinary
  `#` comments included -- they show up in the recording. Useful for narration, easy to
  forget when you write a header comment nobody was meant to see.
- **A control line is for the one command under it.** `#$ delay 15` types that command fast
  and the next one at the usual pace again; there's nothing to reset. Several control lines in
  front of one command all apply to it.
- **Multi-line commands are one command.** A heredoc, a trailing `\`, or a quote left open
  carries the command onto the following lines, which are typed as they stand -- a blank line
  or a `#$` line in there is content, not formatting -- and the control lines in front cover
  all of it.
- **Anything that pages will stall the take.** The recorded session is a real terminal, so
  `git log`, `git diff`, `man` and friends open `less` and wait for a keypress the script never
  types. Waiting can't help here -- the keystrokes that would dismiss the pager are the ones
  being held back -- so the line burns `--cmd-timeout`, warns, and the run then fails on
  `--exit-timeout` instead of finishing. Set `PAGER=cat` (or `GIT_PAGER=cat`, or pass `--no-pager`).
  When the demo genuinely wants one -- an editor, a REPL -- put `#$ handover` in front of it
  and drive it yourself; see `handover.sh`.
- **Mind the `!`.** The recorded shell is interactive, so history expansion is on and
  `echo "done!"` dies with `bash: !": event not found`. Single-quote the string, or start the
  script with `set +H`.
- **`#$ pause` is breathing room, not a runtime guess.** Each command is typed only once the
  previous one has finished, so a slow build needs no padding -- `#$ pause` is the extra beat
  on top, for letting output be read. One at the end of the script holds the final prompt
  before the session ends.
- **Start from a clean slate.** Work in a scratch directory the script clears on its first
  line, or a second take begins from whatever the first one left behind. The same goes for
  config a tool reads from `$HOME` -- neutralise it if the demo should look the same
  everywhere.
- **Blocks are typed line by line.** An `if`/`for`/`while` block or a `{ }` group is several
  commands to asciiscript, one per physical line; blank lines inside are skipped and a `#$`
  line inside applies to the line after it. Heredocs, open quotes and trailing `\` are the
  constructs that are read as one command.
