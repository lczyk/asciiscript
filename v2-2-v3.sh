#!/usr/bin/env bash
# Convert asciicast v2 recordings (asciinema 2.x) to asciicast v3, the format
# asciiscript writes and to-video.sh reads. -h has the options.
#
#   ./v2-2-v3.sh old.cast > new.cast
#   ./v2-2-v3.sh -b .v2 *.cast
#
# Needs jq.

QUIET=""
_SCRATCH=""
_TMP=""

HELP="usage: v2-2-v3.sh [-q] FILE
       v2-2-v3.sh [-q] -i [-b SUFFIX] FILE...
       v2-2-v3.sh [-q] -c FILE...

Convert asciicast v2 recordings (asciinema 2.x) to asciicast v3 (asciinema 3.x).
Given one FILE and no -i, print it converted to stdout; FILE may be - for stdin.

  -i         convert each FILE in place; files already v3 are left alone
  -b SUFFIX  keep each original as FILE+SUFFIX, e.g. -b .v2 (implies -i)
  -c         only report each FILE's version; exit 1 unless all are v3
  -q         print errors only
  -h         show this help

width and height become term.cols and term.rows, theme becomes term.theme,
env.TERM is copied to term.type and duration is dropped; the rest of the
header carries over. Event times become intervals since the previous event,
rounded to the millisecond with the error carried forward, as asciinema writes
them; an event timed earlier than the one before it gets a zero interval.
Event codes and data are copied as they are. No exit event is added: v2 has no
record of the exit status.

Needs jq. 'asciinema convert old.cast new.cast' (asciinema 3.x) converts a
single file too. v3 plays in asciinema 3.0+, asciinema-player 3.10+ and on
asciinema.org; keep the originals (-b) for anything older.

Exits 1 if a FILE couldn't be converted (or, with -c, isn't v3), 2 on bad usage.

examples:
  v2-2-v3.sh old.cast > new.cast
  v2-2-v3.sh -b .v2 *.cast
  find . -name '*.cast' -exec ./v2-2-v3.sh -i {} +
  v2-2-v3.sh -c *.cast"

_VERSION_JQ='if type == "object" and (.version == 1 or .version == 2 or .version == 3)
then .version | floor else empty end'

# Reads a v2 recording on stdin and prints it as v3. How many events had to be
# pulled forward in time goes to stderr, as a bare number, when there are any.
# shellcheck disable=SC2016
_CONVERT_JQ='
# Microseconds in an event time, cut from its decimal digits the way asciinema
# reads them: multiplying out the float lands a microsecond short now and then.
def us:
    tostring | ascii_downcase | split("e") as [$m, $x]
    | ($m | split(".")) as [$i, $f]
    | ($i + ($f // "")) as $d
    | (($i | length) + ($x // "0" | ltrimstr("+") | tonumber) + 6) as $n
    | if $n <= 0 then 0
      else $d + ([range(0; $n - ($d | length))] | map("0") | join("")) | .[0:$n] | tonumber
      end;

def secs: (. / 1000 | floor) as $s | "\($s).\(. - $s * 1000 + 1000 | tostring | .[1:])";

def header:
    if type == "object" and .version == 2 then . else error("not an asciicast v2 header") end
    | if [.width, .height] | map(type == "number" and . >= 1 and . <= 65535) | all then .
      else error("the header has no valid width and height") end
    | (if .env | type == "object"
       then .env | with_entries(select(.value != null) | .value |= tostring)
       else {} end) as $env
    | {version: 3, term: ({cols: (.width | floor), rows: (.height | floor)}
        + (if $env.TERM then {type: $env.TERM} else {} end)
        + (if .theme | type == "object" then {theme} else {} end))}
    + (if .timestamp | type == "number" then {timestamp: (.timestamp | floor)} else {} end)
    + (if .idle_time_limit | type == "number" then {idle_time_limit} else {} end)
    + (if .command != null then {command: (.command | tostring)} else {} end)
    + (if .title != null then {title: (.title | tostring)} else {} end)
    + (if $env != {} then {env: $env} else {} end)
    + del(.version, .width, .height, .term, .theme, .timestamp, .duration,
          .idle_time_limit, .command, .title, .env);

def event:
    if type == "array" and length == 3 and (.[0] | type) == "number"
       and (.[1] | type) == "string" and .[1] != "" and (.[2] | type) == "string"
    then . else error("not an event: \(tojson | .[0:60])") end
    # 2^53 microseconds, past which jq no longer counts them exactly
    | if .[0] < 9007199254 then . else error("event time out of range: \(.[0])") end;

# Intervals are rounded to the millisecond with the rounding error carried into
# the next one, as asciinema quantises them, so the total never drifts.
input | header | tojson,
    foreach ((inputs | [event]), "end") as $e ({t: 0, err: 0, back: 0};
        if $e == "end" then . else
            ($e[0][0] | if . > 0 then us else 0 end) as $us
            | ([$us, .t] | max) as $t
            | ($t - .t + .err) as $c
            | (($c + 500) / 1000 | floor) as $ms
            | {t: $t, err: ($c - $ms * 1000),
               back: (.back + (if $e[0][0] < 0 or $us < .t then 1 else 0 end)),
               line: "[\($ms | secs), \($e[0][1] | tojson), \($e[0][2] | tojson)]"}
        end;
        if $e != "end" then .line elif .back > 0 then .back | stderr | empty else empty end)
'

function _note() {
    [ -n "${QUIET}" ] || printf 'v2-2-v3: %s\n' "$*" >&2
}

function _error() {
    printf 'v2-2-v3: %s\n' "$*" >&2
}

function _usage() {
    _error "$1"
    printf "try 'v2-2-v3.sh -h'\n" >&2
    exit 2
}

function _drop_tmp() {
    [ -z "${_TMP}" ] || rm -f -- "${_TMP}"
    _TMP=""
}

function _cleanup() {
    _drop_tmp
    [ -z "${_SCRATCH}" ] || rm -rf -- "${_SCRATCH}"
}

# _version FILE: print 1, 2 or 3 if FILE is an asciicast of that version
function _version() {
    if [ "$1" = - ]; then
        head -n 1
    else
        head -n 1 < "$1"
    fi | jq -r "${_VERSION_JQ}" 2> /dev/null
}

function _readable() {
    if [ -d "$1" ]; then
        _error "$1: is a directory"
    elif [ ! -e "$1" ]; then
        _error "$1: no such file"
    elif [ ! -r "$1" ]; then
        _error "$1: permission denied"
    else
        return 0
    fi
    return 1
}

function _unsupported() {
    if [ "$2" = 1 ]; then
        _error "$1: asciicast v1, which 'asciinema convert' reads but this doesn't"
    else
        _error "$1: not an asciicast v2 or v3 recording"
    fi
}

# _convert IN OUT NAME: write IN, a v2 recording, to OUT as v3
function _convert() {
    local back
    if ! jq -n -r "${_CONVERT_JQ}" < "$1" > "$2" 2> "${_SCRATCH}/err"; then
        _error "$3: $(sed 's/^jq: error (at <stdin>:\([0-9]*\)): /line \1: /' "${_SCRATCH}/err")"
        return 1
    fi
    back="$(cat "${_SCRATCH}/err")"
    case "${back}" in
        ('' | *[!0-9]*) ;;
        (*) _note "$3: ${back} event(s) timed earlier than the event before, given a zero interval" ;;
    esac
}

function _print() {
    local in="$1" name="$1" v
    if [ "$1" = - ]; then
        name="stdin"
        in="${_SCRATCH}/in"
        cat > "${in}" || return 1
    elif ! _readable "$1"; then
        return 1
    elif [ ! -f "$1" ]; then
        # a pipe can only be read once, and it's read twice below
        in="${_SCRATCH}/in"
        cat < "$1" > "${in}" || return 1
    fi
    v="$(_version "${in}")"
    case "${v}" in
        (3)
            _note "${name}: already v3, printed as is"
            cat < "${in}"
            ;;
        (2)
            _convert "${in}" "${_SCRATCH}/out" "${name}" && cat < "${_SCRATCH}/out"
            ;;
        (*)
            _unsupported "${name}" "${v}"
            return 1
            ;;
    esac
}

function _inplace() {
    local f="$1" backup="" dir v
    [ -z "$2" ] || backup="$1$2"
    if [ -L "${f}" ]; then
        _error "${f}: is a symlink; convert the file it points to"
        return 1
    fi
    _readable "${f}" || return 1
    if [ ! -f "${f}" ]; then
        _error "${f}: not a regular file"
        return 1
    fi
    v="$(_version "${f}")"
    case "${v}" in
        (3)
            _note "${f}: already v3, left alone"
            return 0
            ;;
        (2) ;;
        (*)
            _unsupported "${f}" "${v}"
            return 1
            ;;
    esac
    if [ -n "${backup}" ] && { [ -e "${backup}" ] || [ -L "${backup}" ]; }; then
        _error "${f}: ${backup} already exists"
        return 1
    fi
    case "${f}" in
        (*/*) dir="${f%/*}" ;;
        (*) dir="." ;;
    esac
    case "${dir}" in
        ('') dir="/" ;;
        (-*) dir="./${dir}" ;;
    esac
    # the original is copied in first so the result keeps its permissions
    if ! _TMP="$(mktemp "${dir}/.v2-2-v3.XXXXXX")" || ! cp -p -- "${f}" "${_TMP}"; then
        _error "${f}: can't write next to it"
        _drop_tmp
        return 1
    fi
    if ! _convert "${f}" "${_TMP}" "${f}" \
        || { [ -n "${backup}" ] && ! cp -p -- "${f}" "${backup}"; } \
        || ! mv -f -- "${_TMP}" "${f}"; then
        _drop_tmp
        return 1
    fi
    _TMP=""
    if [ -n "${backup}" ]; then
        _note "${f}: converted, original kept as ${backup}"
    else
        _note "${f}: converted"
    fi
}

function _check() {
    local v
    if [ "$1" != - ]; then
        _readable "$1" || return 1
    fi
    v="$(_version "$1")"
    if [ -z "${QUIET}" ]; then
        case "${v}" in
            ('') printf '%s: not an asciicast\n' "$1" ;;
            (*) printf '%s: v%s\n' "$1" "${v}" ;;
        esac
    fi
    [ "${v}" = 3 ]
}

function main() {
    local inplace="" check="" suffix="" opt f status=0
    case "${1:-}" in
        (--help)
            printf '%s\n' "${HELP}"
            exit 0
            ;;
    esac
    while getopts ':ib:cqh' opt; do
        case "${opt}" in
            (i) inplace=1 ;;
            (b)
                inplace=1
                suffix="${OPTARG}"
                [ -n "${suffix}" ] || _usage "-b needs a suffix"
                ;;
            (c) check=1 ;;
            (q) QUIET=1 ;;
            (h)
                printf '%s\n' "${HELP}"
                exit 0
                ;;
            (:) _usage "-${OPTARG} needs an argument" ;;
            (*) _usage "unknown option -${OPTARG}" ;;
        esac
    done
    shift $((OPTIND - 1))
    [ $# -gt 0 ] || _usage "no FILE given"
    [ -z "${inplace}" ] || [ -z "${check}" ] || _usage "-c doesn't go with -i or -b"
    [ -n "${inplace}" ] || [ -n "${check}" ] || [ $# -eq 1 ] || _usage "one FILE at a time, unless -i"
    if ! command -v jq > /dev/null; then
        _error "jq not found, and it does the converting"
        exit 2
    fi

    if [ -n "${check}" ]; then
        for f in "$@"; do
            _check "${f}" || status=1
        done
        exit "${status}"
    fi

    _SCRATCH="$(mktemp -d)" || exit 2
    if [ -n "${inplace}" ]; then
        for f in "$@"; do
            if [ "${f}" = - ]; then
                _error "-: stdin can't be converted in place"
                status=1
            else
                _inplace "${f}" "${suffix}" || status=1
            fi
        done
        exit "${status}"
    fi
    _print "$1" || exit 1
}

trap _cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

main "$@"
