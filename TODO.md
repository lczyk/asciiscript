# TODO

Working list from the September 2026 design review. Ticked as it lands.

## Recording engine (`record.go`, `main.go`, `handover.go`)

- [x] drop `--settle`: wait for the first prompt marker instead (bounded), factor one prompt wait for startup / sync / handover
- [x] `confirmEcho`: reword the error, only look at output after the line was typed, deadline as a session field so the test doesn't burn 500ms
- [x] `tally`: count the OSC 133 wrapper, not the bare probe string
- [x] `readonly PS1 PS2` in the rcfile so a demo that sets its own prompt fails visibly
- [x] handle SIGHUP alongside SIGINT/SIGTERM so a closed window still stops asciinema
- [x] rcfile path: pass via `$ASCIISCRIPT_RCFILE` instead of splicing it into `--command` (fixes quoting, stable `.cast` header, no temp path in the recording); env instead of `env`
- [x] `stripQueries` only outside a handover, so a handed-over program's terminal queries get answered
- [x] SIGWINCH during a handover resizes the recording pty
- [x] `--version`: drop the pre-scan, check `opts.Version` after `Parse`
- [x] validate `--cols`/`--rows` (1..65535, 0 = default) and validate every numeric flag before looking for asciinema or the script
- [x] default size 80x24; the current terminal's size only when the script has a handover
- [x] warn at start when a script line will wrap at `--cols` (readline bakes the width into the recording)
- [x] `--idle-time-limit` and `--title` passed through to asciinema
- [x] check `asciinema --version` is 3.x at startup
- [x] seed the prompt marker token from `--seed` so `--jitter 0 --seed N` takes are identical
- [x] `finish`'s error is kept when typing was interrupted (`errors.Join`)
- [x] seed line goes through `s.warn`
- [x] script path `-` reads stdin
- [x] `--help` text: `--speed` scales pauses too, no "typed exit", "else 80"/"else 24"
- [x] refuse to run on Windows with a clear message (build tag)

## Parser (`script.go`)

- [x] heredoc delimiter is a bash word: `<<END-OF`, `<<my.marker`, `<<\EOF`
- [x] `$'...'` ANSI-C quoting: `\'` doesn't close it
- [x] `#` after `;` `|` `&` `(` `)` starts a comment, as in bash
- [x] several heredocs on one line are all waited for, in order
- [x] `#$ delay`/`#$ pause`: reject negative, reject overflow, cap at something sane

## Version and packaging

- [x] `//go:embed VERSION` + `ver.Read` (build info) replace the generated `version_gen.go`; `make generate-version` and the gitignore entry go
- [x] `go 1.26` in go.mod, not a patch pin
- [x] drop the dead upx step (upx refuses macOS)
- [x] `.gitignore`: anchor the `*.sh` rule to the root
- [x] LICENSE: keep the 2018 notice, add the rewrite's line; README credits the fork origin
- [x] CHANGELOG.md, back-filled from the tags
- [x] GitHub Actions: `make verify` on ubuntu + macos
- [ ] (user) tag `v0.2.1` at fdcb177 -- the changelog notes it as untagged until then

## Tests

- [x] `record`/`options` validation table test (all branches)
- [x] `checkHandover` takes the terminal check as an argument so the test doesn't depend on how `go test` was launched
- [x] `finish` write-error path
- [x] `termSize` asserts the 80x24 fallback
- [x] `syncWriter.await` -> `assert.Eventually`
- [x] README flags block is checked against the options struct by a test
- [x] test comments: no "old behaviour", no "--fail-fast"

## Docs (`README.md`, `examples/README.md`)

- [x] Install section (make, go install)
- [x] `--settle` gone; the "prompt shows before input is live" claim gone
- [x] `idle_time_limit` is off by default: `--idle-time-limit`
- [x] recording width: readline bakes it in; `asciinema play -r`; default 80x24
- [x] compound commands are typed per line
- [x] TERM/LANG/LC_* are inherited; non-UTF-8 output comes out as U+FFFD
- [x] Windows unsupported; asciinema 3.x is checked
- [x] fork credit; changelog pointer

## Later

- [x] write asciicast v3 directly and host bash ourselves (drops asciinema as a runtime dependency, exact timestamps, `m`/`i` events) -- the 0.5 theme
