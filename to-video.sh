#!/usr/bin/env bash
# Render an asciiscript recording to a video that survives upload to Drive/Slides:
# large font, 16:9 frame, near-lossless h264. The output's extension picks the
# format: .mp4 (default) or .gif.
#
#   ./to-video.sh demo.cast [demo.mp4]
#
# Idle gaps are capped at IDLE seconds, except the ones a `#$ pause` asked for
# (the gap before its marker, and the trailing pause before the shell exits).
# Needs asciinema 3.x, agg, ffmpeg and jq; with a FONT, fontconfig and that font too.
set -e

IDLE="${IDLE:-2}"             # cap on any gap no `#$ pause` asked for, seconds
LEAD="${LEAD:-2}"             # hold on the first prompt before typing starts, seconds
TAIL="${TAIL:-1}"             # hold on the last frame, seconds
THEME="${THEME:-monokai}"
FONT="${FONT-Ubuntu Sans Mono}"  # empty leaves the choice to agg
FONT_SIZE="${FONT_SIZE:-40}"
WIDTH="${WIDTH:-2560}"
HEIGHT="${HEIGHT:-1440}"
CRF="${CRF:-18}"

function _fail() { printf 'to-video: %s\n' "$1" >&2; exit 1; }

function _retime() {
    # the gap before an event is its first field (asciicast v3 intervals)
    jq -cs --argjson idle "${IDLE}" --argjson lead "${LEAD}" '
        .[0] as $header | .[1:] as $ev
        | ([$ev | to_entries[] | select(.value[1] == "o") | .key] | last) as $last
        | $header, ($ev | to_entries[] | .key as $i | .value
            | if .[1] == "m" or $i == $last then .
              else .[0] = ([.[0], $idle] | min) end
            | if $i == 1 then .[0] = ([.[0], $lead] | max) else . end)
    ' "$1"
}

function _background() {
    # sample the terminal's own background, so the 16:9 padding blends in
    ffmpeg -loglevel error -i "$1" -frames:v 1 -vf "crop=1:1:iw/2:ih-20" \
        -f rawvideo -pix_fmt rgb24 - | od -An -tx1 | tr -d ' \n'
}

function main() {
    local in="$1"
    local out="${2:-${1%.cast}.mp4}"
    [ -f "${in}" ] || _fail "usage: to-video.sh <in.cast> [out.mp4|out.gif]"
    case "${out}" in (*.mp4|*.gif) ;; (*) _fail "output must be .mp4 or .gif: ${out}" ;; esac
    head -n 1 "${in}" | grep -q '"version": *3' || _fail "not an asciicast v3 recording: ${in} (./v2-2-v3.sh converts v2)"
    for tool in asciinema agg ffmpeg jq; do
        command -v "${tool}" >/dev/null || _fail "${tool} not found"
    done
    local font=()
    if [ -n "${FONT}" ]; then
        command -v fc-list >/dev/null || _fail "fc-list not found"
        # agg falls back to its own font list without a word
        [ -n "$(fc-list "${FONT}")" ] || _fail "font not installed: ${FONT}"
        font=(--font-family "${FONT}")
    fi

    local tmp
    tmp="$(mktemp -d)"
    trap 'rm -rf "'"${tmp}"'"' EXIT

    _retime "${in}" > "${tmp}/retimed.cast"
    # agg reads asciicast v2 only
    asciinema convert -f asciicast-v2 --overwrite "${tmp}/retimed.cast" "${tmp}/v2.cast" >/dev/null
    # the idle cap is already applied above; agg's own would flatten the pauses too
    agg --theme "${THEME}" "${font[@]}" --font-size "${FONT_SIZE}" --idle-time-limit 3600 \
        --last-frame-duration "${TAIL}" "${tmp}/v2.cast" "${tmp}/out.gif" >/dev/null

    if [ "${out}" != "${out%.gif}" ]; then
        cp "${tmp}/out.gif" "${out}"
    else
        # fps before anything else: the gif demuxer drops the last frame's hold otherwise.
        # crop shaves agg's rounded window corners; neighbor keeps glyph edges hard.
        ffmpeg -y -loglevel error -i "${tmp}/out.gif" -vf "fps=30,crop=iw-16:ih-16,\
scale=${WIDTH}:${HEIGHT}:force_original_aspect_ratio=decrease:flags=neighbor,\
pad=${WIDTH}:${HEIGHT}:(ow-iw)/2:(oh-ih)/2:color=0x$(_background "${tmp}/out.gif")" \
            -c:v libx264 -crf "${CRF}" -preset slow -tune stillimage \
            -pix_fmt yuv420p -movflags faststart "${out}"
    fi
    printf 'to-video: wrote %s\n' "${out}"
}

main "$@"
