#!/usr/bin/env bash
#
# blcheck.sh - check IP(s) against ~60 DNS blacklists via mxtoolbox's web API.
#
#   Single IP : ./blcheck.sh 51.83.226.103
#   From file : ./blcheck.sh -f targets.txt      -> writes targets.txt-BL
#
# List lines may be any of:
#   1.2.3.4
#   1.2.3.4:3128
#   http://1.2.3.4:3128
#   socks5://1.2.3.4:1080 [42ms]
# Blank lines and lines with no IPv4 are copied through untouched.
#
# No deps beyond bash + curl + grep (PCRE). No jq.

set -uo pipefail

# ---- config / defaults -------------------------------------------------------
UA='Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36'
BASE='https://mxtoolbox.com'
TOKEN="${MXTB_TOKEN:-}"     # env override; else auto-fetched
DELAY=2                      # seconds between requests in file mode
VERBOSE=0
NOCOLOR=0
FILE=''
SINGLE=''

# ---- colors ------------------------------------------------------------------
setup_colors() {
  if [[ $NOCOLOR -eq 1 || ! -t 1 ]]; then
    R='' G='' Y='' DIM='' B='' N=''
  else
    R=$'\033[0;31m' G=$'\033[0;32m' Y=$'\033[0;33m'
    DIM=$'\033[2m'  B=$'\033[1m'    N=$'\033[0m'
  fi
}

usage() {
  cat <<EOF
Usage:
  ${0##*/} [options] <ip>
  ${0##*/} [options] -f <file>

Options:
  -f, --file FILE     Check every address in FILE (one per line).
                      Writes FILE-BL: original lines + blacklist verdict appended.
  -t, --token TOK     mxtoolbox tempauthorization token.
                      Precedence: -t > \$MXTB_TOKEN > auto-fetch from /api/v1/user
  -d, --delay SEC     Delay between lookups in file mode (default: ${DELAY}).
  -v, --verbose       Single-IP mode: list every blacklist (green OK / red LISTED).
  -n, --no-color      Disable colored output.
  -h, --help          This help.
EOF
}

# ---- arg parsing -------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    -f|--file)    FILE="${2:-}"; shift 2 ;;
    -t|--token)   TOKEN="${2:-}"; shift 2 ;;
    -d|--delay)   DELAY="${2:-}"; shift 2 ;;
    -v|--verbose) VERBOSE=1; shift ;;
    -n|--no-color) NOCOLOR=1; shift ;;
    -h|--help)    usage; exit 0 ;;
    -*)           echo "Unknown option: $1" >&2; usage; exit 1 ;;
    *)            SINGLE="$1"; shift ;;
  esac
done
setup_colors

if [[ -z "$FILE" && -z "$SINGLE" ]]; then
  echo "Error: give an IP or -f <file>." >&2; usage; exit 1
fi

# ---- token -------------------------------------------------------------------
fetch_token() {
  curl -s "${BASE}/api/v1/user" \
    -H "user-agent: ${UA}" \
    -H 'x-requested-with: XMLHttpRequest' \
    | grep -oP 'TempAuthKey"\s*:\s*"\K[0-9a-fA-F-]{36}' | head -1
}

ensure_token() {
  [[ -n "$TOKEN" ]] && return 0
  TOKEN="$(fetch_token)"
  if [[ -z "$TOKEN" ]]; then
    echo "${R}Error:${N} could not obtain a temp token. Pass one with -t or set \$MXTB_TOKEN." >&2
    exit 2
  fi
}

# ---- helpers -----------------------------------------------------------------
# Pull first IPv4 out of any of the supported line formats.
extract_ip() {
  local line="${1#*://}"          # drop protocol scheme if present
  grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' <<<"$line" | head -1
}

# Raw API call for one IP.
lookup_raw() {
  local ip="$1"
  curl -s "${BASE}/api/v1/Lookup?command=blacklist&argument=${ip}&resultIndex=6&disableRhsbl=true&format=2" \
    -H 'accept: application/json, text/javascript, */*; q=0.01' \
    -H 'content-type: application/json; charset=utf-8' \
    -H "referer: ${BASE}/SuperTool.aspx" \
    -H "tempauthorization: ${TOKEN}" \
    -H "user-agent: ${UA}" \
    -H 'x-requested-with: XMLHttpRequest'
}

# Globals set by parse_result:
#   RES_TOTAL RES_LISTED RES_NAMES(newline-sep)  RES_OK(bool: parsed ok)
parse_result() {
  local resp="$1"
  RES_TOTAL='' RES_LISTED='' RES_NAMES='' RES_OK=0
  resp="$(tr '\n' ' ' <<<"$resp")"

  RES_TOTAL="$(grep -oP 'against <strong>\K[0-9]+' <<<"$resp" | head -1 || true)"
  RES_LISTED="$(grep -oP 'Listed <strong>\K[0-9]+' <<<"$resp" | head -1 || true)"

  [[ -z "$RES_TOTAL" ]] && return 1   # parse failed / API error
  [[ -z "$RES_LISTED" ]] && RES_LISTED=0

  # names of blacklists whose row is LISTED
  RES_NAMES="$(grep -oP '<tr>.*?</tr>' <<<"$resp" \
                | grep '&nbsp;LISTED' \
                | grep -oP 'bld_name\\">\K[^<]+' || true)"
  RES_OK=1
  return 0
}

# ---- single IP mode ----------------------------------------------------------
run_single() {
  local ip; ip="$(extract_ip "$1")"
  if [[ -z "$ip" ]]; then echo "${R}No IPv4 found in:${N} $1" >&2; exit 1; fi
  ensure_token

  local resp; resp="$(lookup_raw "$ip")"
  if ! parse_result "$resp"; then
    echo "${Y}?${N} ${B}${ip}${N}  API error / no result (token expired? try -t)" >&2
    exit 3
  fi

  if [[ "$RES_LISTED" -gt 0 ]]; then
    printf '%s%s%s  %s[%s/%s]%s  %sLISTED%s\n' \
      "$R" "$ip" "$N" "$B" "$RES_LISTED" "$RES_TOTAL" "$N" "$R" "$N"
    while IFS= read -r name; do
      [[ -n "$name" ]] && printf '  %s✗%s %s\n' "$R" "$N" "$name"
    done <<<"$RES_NAMES"
  else
    printf '%s%s%s  %s[0/%s]%s  %sclean%s\n' \
      "$G" "$ip" "$N" "$B" "$RES_TOTAL" "$N" "$G" "$N"
  fi

  # -v: dump every blacklist with per-row status
  if [[ $VERBOSE -eq 1 ]]; then
    local flat; flat="$(tr '\n' ' ' <<<"$resp")"
    grep -oP '<tr>.*?</tr>' <<<"$flat" | while IFS= read -r row; do
      local name status
      name="$(grep -oP 'bld_name\\">\K[^<]+' <<<"$row" | head -1)"
      [[ -z "$name" ]] && continue
      if   grep -q '&nbsp;LISTED'  <<<"$row"; then status="${R}LISTED ${N}"
      elif grep -q 'TIMEOUT'       <<<"$row"; then status="${Y}TIMEOUT${N}"
      else                                         status="${G}OK     ${N}"
      fi
      printf '    %b  %s%s%s\n' "$status" "$DIM" "$name" "$N"
    done
  fi
}

# ---- file mode ---------------------------------------------------------------
run_file() {
  local infile="$1"
  [[ -f "$infile" ]] || { echo "${R}No such file:${N} $infile" >&2; exit 1; }
  ensure_token
  local outfile="${infile}-BL"
  : > "$outfile"

  local total_lines; total_lines="$(wc -l < "$infile")"
  local i=0
  while IFS= read -r line || [[ -n "$line" ]]; do
    i=$((i+1))
    local ip; ip="$(extract_ip "$line")"

    if [[ -z "$ip" ]]; then
      printf '%s\n' "$line" >> "$outfile"          # passthrough
      continue
    fi

    local resp; resp="$(lookup_raw "$ip")"
    if ! parse_result "$resp"; then
      printf '%s [Blacklisted: ERROR]\n' "$line" >> "$outfile"
      printf '%s[%s/%s]%s %s%-21s%s %sERROR%s\n' \
        "$DIM" "$i" "$total_lines" "$N" "$B" "$ip" "$N" "$Y" "$N" >&2
      sleep "$DELAY"; continue
    fi

    # names -> ", "-joined (skip blank lines)
    local names_csv=''
    names_csv="$(awk 'NF{a[++n]=$0} END{for(i=1;i<=n;i++)printf "%s%s",(i>1?", ":""),a[i]}' <<<"$RES_NAMES")"

    printf '%s [Blacklisted: %s/%s [%s]]\n' \
      "$line" "$RES_LISTED" "$RES_TOTAL" "$names_csv" >> "$outfile"

    # live progress on stderr
    if [[ "$RES_LISTED" -gt 0 ]]; then
      printf '%s[%s/%s]%s %s%-21s%s %s%s/%s%s %s\n' \
        "$DIM" "$i" "$total_lines" "$N" "$B" "$ip" "$N" \
        "$R" "$RES_LISTED" "$RES_TOTAL" "$N" "$names_csv" >&2
    else
      printf '%s[%s/%s]%s %s%-21s%s %s0/%s clean%s\n' \
        "$DIM" "$i" "$total_lines" "$N" "$B" "$ip" "$N" "$G" "$RES_TOTAL" "$N" >&2
    fi
    sleep "$DELAY"
  done < "$infile"

  echo >&2
  echo "${G}Done.${N} Results written to ${B}${outfile}${N}" >&2
}

# ---- dispatch ----------------------------------------------------------------
if [[ -n "$FILE" ]]; then run_file "$FILE"; else run_single "$SINGLE"; fi
