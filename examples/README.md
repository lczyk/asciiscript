# Examples

Ready-to-record scripts. Record any of them the usual way:

```sh
$ asciiscript examples/hello.sh hello.cast
$ asciinema play hello.cast
```

- **`hello.sh`** -- the README demo. The smallest thing that shows both control commands.
- **`pacing.sh`** -- `#$ delay` and `#$ wait` in anger: fast lines, slow lines, and giving
  output room to breathe.
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
$ asciiscript --seed 12345 examples/git.sh out.cast      # reproduce a take you liked
```

## Writing your own

- **Only `#$` lines are control lines.** Every other line is typed into the shell, ordinary
  `#` comments included -- they show up in the recording. Useful for narration, easy to
  forget when you write a header comment nobody was meant to see.
- **Anything that pages will hang.** The recorded session is a real terminal, so `git log`,
  `git diff`, `man` and friends open `less` and wait for a keypress the script never types.
  Set `PAGER=cat` (or `GIT_PAGER=cat`, or pass `--no-pager`).
- **Mind the `!`.** The recorded shell is interactive, so history expansion is on and
  `echo "done!"` dies with `bash: !": event not found`. Single-quote the string, or start the
  script with `set +H`.
- **Set `#$ wait` above the command's runtime.** Too low and the next line gets typed while
  the previous command is still running.
- **Start from a clean slate.** Work in a scratch directory the script clears on its first
  line, or a second take begins from whatever the first one left behind. The same goes for
  config a tool reads from `$HOME` -- neutralise it if the demo should look the same
  everywhere.
