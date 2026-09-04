# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `--capture-input`: the keystrokes the script types are recorded as input events. A
  handover's never are.
- A `#$ pause` in front of a command is a marker in the recording, named after that command;
  `asciinema play -m` pauses there. The trailing pause at the end of a script has no command
  and gets no marker.
- A resize during a handover is recorded as a resize event.

### Changed

- asciiscript writes the asciicast v3 file itself: bash runs directly in the pty, and the
  session's output is timestamped and written by asciiscript. asciinema is no longer needed to
  record, only to play. The recording's header names the shell that ran and the script;
  intervals are rounded the way asciinema rounds them.
- `--exit-timeout` is how long the shell gets to exit, not asciinema.
- The seed printed on start is four digits, so it can be read off the screen and typed back.
- The recorded shell runs without readline. The terminal echoes what is typed, so a line that
  wraps does so wherever the recording is played rather than at the width it was made, and a
  tab in a script is typed as a tab rather than completed.

### Removed

- The asciinema version check on start, and the first-line echo check: there is nothing
  between asciiscript and the shell any more.
- The warning about lines that would wrap, since wrapping no longer ties a recording to a
  width.

## [0.5.0] - 2026-09-02

### Added

- `--idle-time-limit` and `--title`, written into the recording.
- The script can be `-` for standard input.
- A warning at the start of a take for lines that will wrap at the recording's width, since a
  wrapped line only plays back cleanly at the width it was recorded at.
- The asciinema on `PATH` is checked to be 3.x on start.
- The terminal window closing (SIGHUP) stops the recording cleanly, like `Ctrl-C`.
- Resizing the terminal during a `#$ handover` resizes the recording.
- A handed-over program's terminal queries reach the real terminal, so they get answered.
- `CHANGELOG.md`, a GitHub Actions workflow running `make verify`, and an Install section in the
  README.

### Changed

- Typing starts when the recorded shell shows its first prompt, instead of after a fixed
  `--settle` wait. Recordings no longer open with a second or two of dead air.
- The recording is 80x24 by default, whatever terminal it is made from. A script with a
  `#$ handover` is recorded at the current terminal's size, as before.
- The prompt shows the working directory abbreviated the way fish's `prompt_pwd` does
  (`~/g/asciiscript`) rather than in full, and `PS1`/`PS2` are read-only in the recorded shell.
- `--version` reports the version the binary was built from (the git tag, a pseudo-version
  with the commit, or the `VERSION` file where there is no git), with no generate step.
- The rcfile path reaches the recorded shell through the environment, so the `.cast` header's
  `command` field is the same for every run and a temp directory with a space in it works.
- The prompt marker is derived from `--seed`, so a repeated take carries the same marker.
- `--help` describes `--speed`, `--exit-timeout`, `--cols` and `--rows` accurately.

### Removed

- `--settle`.
- Windows builds: the program needs a Unix pty and now says so instead of failing inside the
  recording.

### Fixed

- `-qv` printed no version and started a recording; `--version` is now honoured however it is
  combined.
- `--cols`/`--rows` outside 1..65535 silently wrapped into another size; they are rejected.
- A bad numeric flag is reported first, rather than masked by a missing script or asciinema.
- Heredoc delimiters are read as bash words, so `<<END-OF-FILE`, `<<my.marker` and `<<\EOF` work;
  several heredocs on one line are all waited for, in order.
- `$'...'` quoting honours a backslash-escaped quote inside it.
- A `#` right after `;`, `|`, `&`, `(` or `)` starts a comment, as in bash.
- `#$ delay` and `#$ pause` reject negative values and values over an hour, instead of typing
  instantly or overflowing.
- Output that happens to contain the prompt marker's text (`echo "$PS1"`) no longer counts as a
  prompt.
- The first-line echo check only looks at output from after the line was typed.
- `finish`'s error is reported alongside an interrupt's, and the jitter seed line goes through
  the same channel as the other diagnostics.

## [0.4.0] - 2026-08-29

### Changed

- Control lines (`#$ delay`, `#$ pause`, `#$ handover`) now apply only to the command written
  directly under them, rather than being inherited by every later line.
- `#$ wait` renamed to `#$ pause`. It now holds the prompt before typing the next command rather
  than pacing after the previous one, is jittered like typing itself, and scales with `--speed`.
  A trailing `#$ pause` holds the final prompt before the session ends; a trailing `#$ delay` or
  `#$ handover`, or a command that never finishes, is now a parse error.
- The script now ends by sending end-of-input (ctrl-d) rather than typing `exit`.

### Removed

- `--wait` flag. Use `#$ pause` instead, which now works from the first line.

### Fixed

- A control line's argument is recognized regardless of how much whitespace precedes it;
  previously more than one space was read as a missing argument.

## [0.3.0] - 2026-08-29

### Changed

- `--timeout` renamed to `--exit-timeout`.

### Removed

- `--no-sync` flag. Every run now waits for the shell prompt between commands; there is no
  longer a mode that types commands without waiting.

## [0.2.1] - 2026-08-28

No git tag exists for this release; it corresponds to commit `fdcb177`.

### Fixed

- A script byte that is not valid UTF-8 is now typed byte for byte instead of being corrupted
  into the Unicode replacement character.

## [0.2.0] - 2026-08-28

Initial tagged release.

### Added

- Host `asciinema rec` in a pty and type a bash script's commands into it, producing a real
  interactive recording (targets asciinema 3.x / asciicast v3).
- Human-like typing timing, on by default and controlled by `--jitter` (digraph-aware pauses
  and occasional hesitation, seeded and reproducible via `--seed`). Earlier in this project's
  history typing jitter was an opt-in `--human` flag with a typo-and-correction model; the
  correction model was dropped and jitter became `--jitter`, on by default, before this release.
- `#$ delay` and `#$ wait` control lines to set typing speed and the pause after a command.
- `#$ handover` control line, which hands the keyboard to a person for commands nothing can
  drive automatically (editors, `ssh`, REPLs).
- Waiting for the shell prompt to return before typing the next command, so a slow command no
  longer needs a hand-tuned `#$ wait`.
- `--cols`, `--rows`, `--settle`, `--wait`, `--speed`, `--quiet`, `--jitter`, `--seed`,
  `--timeout`, `--no-sync`, and `--cmd-timeout` flags.
- `--version` / `-v` flag, stamped from a `VERSION` file at build time.
