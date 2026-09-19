#!/usr/bin/env bash
#
# proxycheck.sh — check IP(s)/proxies against whatismyipaddress.com/proxy-check
#
# Usage:
#   ./proxycheck.sh -i <ip|ip:port|proto://ip:port>
#   ./proxycheck.sh -f <list-file> [-o <output-file>] [-t <timeout>] [-d <delay>]
#
# List file accepts, one entry per line (blank lines and #comments ignored):
#   1.2.3.4
#   1.2.3.4:8080
#   http://1.2.3.4:8080
#   socks5://1.2.3.4:1080 [230ms]
#
# Output for -f mode is written to "<list-file>-PX" unless -o is given.

set -uo pipefail

TARGET_URL="https://whatismyipaddress.com/proxy-check"
TIMEOUT=8
DELAY=0
OUT_FILE=""
MODE=""
SINGLE_ARG=""
LIST_FILE=""

RED=$'\033[0;31m'
GREEN=$'\033[0;32m'
YELLOW=$'\033[0;33m'
CYAN=$'\033[0;36m'
BOLD=$'\033[1m'
NC=$'\033[0m'

usage() {
    cat <<EOF
${BOLD}proxycheck.sh${NC} — proxy/anonymity checker via whatismyipaddress.com

  -i, --ip <target>       Single check. Accepts: ip | ip:port | proto://ip:port
  -f, --file <path>       Bulk check from file (one entry per line)
  -o, --output <path>     Output file for bulk mode (default: <file>-PX)
  -t, --timeout <sec>     Curl connect/max timeout (default: ${TIMEOUT})
  -d, --delay <sec>       Delay between requests in bulk mode (default: ${DELAY})
  -h, --help              Show this help

Entry formats understood in list files:
  1.2.3.4
  1.2.3.4:8080
  http://1.2.3.4:8080
  socks5://1.2.3.4:1080 [230ms]
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -i|--ip)     MODE="single"; SINGLE_ARG="${2:-}"; shift 2 ;;
        -f|--file)   MODE="list"; LIST_FILE="${2:-}"; shift 2 ;;
        -o|--output) OUT_FILE="${2:-}"; shift 2 ;;
        -t|--timeout) TIMEOUT="${2:-}"; shift 2 ;;
        -d|--delay)  DELAY="${2:-}"; shift 2 ;;
        -h|--help)   usage; exit 0 ;;
        *) echo "Unknown argument: $1" >&2; usage; exit 1 ;;
    esac
done

if [[ -z "$MODE" ]]; then
    usage
    exit 1
fi

# --- parse a single line into "proto ip port" or return 1 if invalid/comment/blank ---
parse_entry() {
    local raw="$1"
    local line proto ip port

    line="${raw%%#*}"                      # strip trailing comment
    line="$(echo -n "$line" | sed -E 's/\[[0-9]+[[:space:]]*ms\]//I')"  # strip [123ms]
    line="$(echo -n "$line" | sed -E 's/^[[:space:]]+|[[:space:]]+$//g')" # trim

    [[ -z "$line" ]] && return 1

    if [[ "$line" =~ ^([A-Za-z0-9]+)://([0-9]{1,3}(\.[0-9]{1,3}){3}):([0-9]{1,5})$ ]]; then
        proto="${BASH_REMATCH[1]}"
        ip="${BASH_REMATCH[2]}"
        port="${BASH_REMATCH[4]}"
    elif [[ "$line" =~ ^([0-9]{1,3}(\.[0-9]{1,3}){3}):([0-9]{1,5})$ ]]; then
        proto="http"
        ip="${BASH_REMATCH[1]}"
        port="${BASH_REMATCH[3]}"
    elif [[ "$line" =~ ^([0-9]{1,3}(\.[0-9]{1,3}){3})$ ]]; then
        proto="http"
        ip="${BASH_REMATCH[1]}"
        port="80"
    else
        return 1
    fi

    echo "${proto,,} ${ip} ${port}"
    return 0
}

# --- extract a TRUE/FALSE field from the result table by its label ---
extract_field() {
    local html="$1" label="$2"
    echo "$html" | grep -oP "(?<=${label}&nbsp;Test:&nbsp;&nbsp;</td><td><span style=\"color:#[0-9A-Fa-f]{6};\">)(TRUE|FALSE)" | head -n1
}

colorize_bool() {
    # FALSE = clean/good = green, TRUE = flagged/bad = red
    local v="$1"
    if [[ "$v" == "TRUE" ]]; then
        echo -n "${RED}${v}${NC}"
    elif [[ "$v" == "FALSE" ]]; then
        echo -n "${GREEN}${v}${NC}"
    else
        echo -n "${YELLOW}n/a${NC}"
    fi
}

# --- perform the actual check. sets globals: STATUS, DET_IP, RDNS, WIMIA, TOR, LOC, HDR ---
do_check() {
    local proto="$1" ip="$2" port="$3"
    local html curl_rc

    html="$(curl -s -L --max-time "$TIMEOUT" \
        --proxy "${proto}://${ip}:${port}" \
        -H 'accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8' \
        -H 'accept-language: en-US,en;q=0.9' \
        -H 'cache-control: no-cache' \
        -H 'pragma: no-cache' \
        -H 'upgrade-insecure-requests: 1' \
        -H 'user-agent: Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36' \
        "$TARGET_URL" 2>/dev/null)"
    curl_rc=$?

    DET_IP="" RDNS="" WIMIA="" TOR="" LOC="" HDR=""

    if [[ $curl_rc -ne 0 || -z "$html" ]]; then
        STATUS="FAIL"
        return
    fi

    if echo "$html" | grep -qiE 'just a moment|attention required|cf-browser-verification'; then
        STATUS="BLOCKED"
        return
    fi

    DET_IP="$(echo "$html" | grep -oP '(?<=<td>IP:&nbsp;&nbsp;</td><td>)[^<]+' | head -n1)"
    RDNS="$(extract_field "$html" "rDNS")"
    WIMIA="$(extract_field "$html" "WIMIA")"
    TOR="$(extract_field "$html" "Tor")"
    LOC="$(extract_field "$html" "Loc")"
    HDR="$(extract_field "$html" "Header")"

    if echo "$html" | grep -qi "Proxy server detected"; then
        STATUS="DETECTED"
    elif [[ -n "$WIMIA" ]]; then
        STATUS="CLEAN"
    else
        STATUS="UNKNOWN"
    fi
}

print_single_pretty() {
    local target="$1"
    echo -e "${BOLD}Target:${NC}      $target"
    case "$STATUS" in
        DETECTED) echo -e "${BOLD}Verdict:${NC}     ${RED}PROXY DETECTED${NC}" ;;
        CLEAN)    echo -e "${BOLD}Verdict:${NC}     ${GREEN}NO PROXY DETECTED${NC}" ;;
        BLOCKED)  echo -e "${BOLD}Verdict:${NC}     ${YELLOW}BLOCKED (Cloudflare challenge)${NC}" ;;
        FAIL)     echo -e "${BOLD}Verdict:${NC}     ${YELLOW}CONNECTION FAILED${NC}" ;;
        *)        echo -e "${BOLD}Verdict:${NC}     ${YELLOW}UNKNOWN${NC}" ;;
    esac
    [[ -n "$DET_IP" ]] && echo -e "${BOLD}Seen IP:${NC}     $DET_IP"
    if [[ -n "$WIMIA" ]]; then
        echo -e "${BOLD}rDNS Test:${NC}   $(colorize_bool "$RDNS")"
        echo -e "${BOLD}WIMIA Test:${NC}  $(colorize_bool "$WIMIA")"
        echo -e "${BOLD}Tor Test:${NC}    $(colorize_bool "$TOR")"
        echo -e "${BOLD}Loc Test:${NC}    $(colorize_bool "$LOC")"
        echo -e "${BOLD}Header Test:${NC} $(colorize_bool "$HDR")"
    fi
}

# ============================ SINGLE MODE ============================
if [[ "$MODE" == "single" ]]; then
    parsed="$(parse_entry "$SINGLE_ARG")" || { echo "Invalid target format: $SINGLE_ARG" >&2; exit 1; }
    read -r proto ip port <<< "$parsed"

    do_check "$proto" "$ip" "$port"
    print_single_pretty "${proto}://${ip}:${port}"

    if [[ "$STATUS" == "DETECTED" ]]; then
        echo -e "\n${RED}[found 1/1]${NC}"
    else
        echo -e "\n${GREEN}[not found 0/1]${NC}"
    fi
    exit 0
fi

# ============================= LIST MODE ==============================
if [[ "$MODE" == "list" ]]; then
    [[ -f "$LIST_FILE" ]] || { echo "File not found: $LIST_FILE" >&2; exit 1; }
    OUT_FILE="${OUT_FILE:-${LIST_FILE}-PX}"
    : > "$OUT_FILE"

    mapfile -t lines < "$LIST_FILE"
    total=0
    for l in "${lines[@]}"; do
        parse_entry "$l" >/dev/null && ((total++))
    done

    if [[ $total -eq 0 ]]; then
        echo "No valid entries found in $LIST_FILE" >&2
        exit 1
    fi

    idx=0
    detected=0
    clean=0
    blocked=0
    failed=0

    for l in "${lines[@]}"; do
        parsed="$(parse_entry "$l")" || continue
        read -r proto ip port <<< "$parsed"
        ((idx++))

        do_check "$proto" "$ip" "$port"

        target="${proto}://${ip}:${port}"
        case "$STATUS" in
            DETECTED) color="$RED";    ((detected++)) ;;
            CLEAN)    color="$GREEN";  ((clean++)) ;;
            BLOCKED)  color="$YELLOW"; ((blocked++)) ;;
            FAIL)     color="$YELLOW"; ((failed++)) ;;
            *)        color="$YELLOW" ;;
        esac

        printf "[%d/%d] %-32s -> %s%s%s\n" "$idx" "$total" "$target" "$color" "$STATUS" "$NC"
        echo "${l} => ${STATUS}${DET_IP:+ (seen: $DET_IP)}" >> "$OUT_FILE"

        [[ "$DELAY" != "0" ]] && sleep "$DELAY"
    done

    echo
    echo -e "${BOLD}Summary:${NC} ${total} checked | ${RED}${detected} detected${NC} | ${GREEN}${clean} clean${NC} | ${YELLOW}${blocked} blocked${NC} | ${YELLOW}${failed} failed${NC}"
    echo -e "Results written to: ${CYAN}${OUT_FILE}${NC}"
    exit 0
fi
