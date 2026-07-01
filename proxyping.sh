#!/usr/bin/env bash
#
# proxyping — fetch a proxy list, measure each proxy's average delay through
#             a real request, log results live, and cache the working ones
#             (fastest -> slowest). Runs until every ping returns or times out.
#
set -uo pipefail
export LC_ALL=C            # force '.' decimal in curl's -w, not locale ','

# ---- defaults -------------------------------------------------------------
SRC_URL='https://api.proxyscrape.com/v4/free-proxy-list/get?request=display_proxies&proxy_format=protocolipport&format=text'
OUT='/tmp/proxy.list'
TEST_URL='http://cp.cloudflare.com/generate_204'   # tiny 204, works over plain http
INFILE=''
JOBS=200            # parallelism
TRIES=2            # requests averaged per proxy
TIMEOUT=60         # per-ping timeout in seconds (connect + transfer)
SHOW_ALL=0         # -a : also log DEAD proxies live

usage() {
  cat >&2 <<'EOF'
proxyping — fetch proxies, measure delay, log live, cache fastest -> slowest

usage: proxyping [options]
  -o FILE   output list file             (default /tmp/proxy.list)
  -i FILE   read proxies from FILE instead of downloading
  -u URL    source URL for the proxy list
  -t URL    test URL used to measure delay
  -j N      parallel jobs                (default 50)
  -n N      requests averaged per proxy  (default 2)
  -w SEC    per-ping timeout             (default 60)
  -a        also log DEAD proxies to the console
  -h        this help

Runs until every proxy has answered or hit the per-ping timeout — no overall
deadline. Each worker persists its own hit to a temp file the moment it
returns; when the run ends the temp file is sorted by delay into FILE
(overwritten fresh).  Output format:  protocol://ip:port [Nms]
EOF
  exit "${1:-0}"
}

while getopts 'o:i:u:t:j:n:w:ah' opt; do
  case "$opt" in
    o) OUT=$OPTARG ;;
    i) INFILE=$OPTARG ;;
    u) SRC_URL=$OPTARG ;;
    t) TEST_URL=$OPTARG ;;
    j) JOBS=$OPTARG ;;
    n) TRIES=$OPTARG ;;
    w) TIMEOUT=$OPTARG ;;
    a) SHOW_ALL=1 ;;
    h) usage 0 ;;
    *) usage 1 ;;
  esac
done

command -v curl >/dev/null || { echo 'need curl' >&2; exit 1; }

# ---- worker ---------------------------------------------------------------
# Each ping gets up to $TIMEOUT seconds (curl --max-time). Persists OK hits
# straight to $RES (atomic short-line append) and echoes a status line:
#   OK|<avg_ms>|<proxy>   or   DEAD|<proxy>
test_proxy() {
  local proxy=$1 total=0 ok=0 t i avg
  for ((i=0; i<TRIES; i++)); do
    if t=$(curl -s -o /dev/null \
             --proxy "$proxy" \
             --connect-timeout "$TIMEOUT" \
             --max-time "$TIMEOUT" \
             -w '%{time_total}' \
             "$TEST_URL" 2>/dev/null); then
      total=$(awk -v a="$total" -v b="$t" 'BEGIN{print a+b}')
      ok=$((ok+1))
    fi
  done
  if (( ok > 0 )); then
    avg=$(awk -v tot="$total" -v n="$ok" 'BEGIN{ printf "%d", (tot/n)*1000 }')
    printf '%s\t%s\n' "$avg" "$proxy" >> "$RES"      # persist immediately
    printf 'OK|%s|%s\n' "$avg" "$proxy"              # live log
  else
    printf 'DEAD|%s\n' "$proxy"
  fi
  return 0
}
export -f test_proxy
export TRIES TIMEOUT TEST_URL

# ---- gather input ---------------------------------------------------------
tmp=$(mktemp); RES=$(mktemp); export RES
trap 'rm -f "$tmp" "$RES"' EXIT

if [[ -n $INFILE ]]; then cat -- "$INFILE"; else curl -fsSL "$SRC_URL"; fi \
  | tr -d '\r' \
  | grep -E '^(socks5|socks4|https?)://[0-9.]+:[0-9]+$' \
  | sort -u > "$tmp"

count=$(wc -l < "$tmp")
(( count > 0 )) || { echo 'no proxies to test' >&2; exit 1; }

if [[ -t 1 ]]; then G=$'\e[32m'; R=$'\e[31m'; D=$'\e[2m'; Z=$'\e[0m'
else G= R= D= Z=; fi

echo "testing $count proxies (jobs=$JOBS tries=$TRIES, ${TIMEOUT}s per ping) ..." >&2

# ---- run: wait for ALL pings (each capped at $TIMEOUT); no overall deadline
xargs -P "$JOBS" -I{} bash -c 'test_proxy "$@"' _ {} < "$tmp" \
| { i=0; ok=0
    while IFS='|' read -r status f2 f3; do
      i=$((i+1))
      if [[ $status == OK ]]; then
        ok=$((ok+1))
        printf '\r\e[2K%s OK %s %s [%sms]\n' "$G" "$Z" "$f3" "$f2"
      elif (( SHOW_ALL )); then
        printf '\r\e[2K%sDEAD%s %s\n' "$R" "$Z" "$f2"
      fi
      printf '\r%stested %d/%d  working %d%s' "$D" "$i" "$count" "$ok" "$Z" >&2
    done
    printf '\n' >&2
  }

# ---- final: sort temp by delay -> fresh output file -----------------------
sort -n "$RES" | awk -F'\t' '{ printf "%s [%sms]\n", $2, $1 }' > "$OUT"

working=$(wc -l < "$RES")
echo "done: $working/$count working, sorted by delay -> $OUT" >&2
