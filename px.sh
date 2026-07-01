#!/usr/bin/env bash
#
# SilkRouter (px) — build a fast local proxy cache and route commands through it.
#
#   px.sh <command...>     run a command through a random cached proxy
#   px.sh -B               (re)build the cache and exit
#
# On first run (no cache) it offers to build a tested, sorted cache. Decline
# and it grabs a fresh proxy straight from the source instead.
#
set -uo pipefail
export LC_ALL=C            # force '.' decimal in curl's -w, not locale ','

# ---- shared defaults ------------------------------------------------------
SRC_URL='https://api.proxyscrape.com/v4/free-proxy-list/get?request=display_proxies&proxy_format=protocolipport&format=text'
LIST=${PX_LIST:-/tmp/proxy.list}
TEST_URL='http://cp.cloudflare.com/generate_204'   # tiny 204 for delay tests
# build params
JOBS=2000          # parallel workers
TRIES=2            # samples averaged per proxy
PING_TIMEOUT=60    # per-ping timeout (connect + transfer)
SHOW_ALL=0         # -a : log DEAD proxies while building
# run params
LO=0               # min delay ms
HI=1000            # max delay ms
WANT=any           # http | socks | any
FORCE=""           # -p : specific proxy
VTIMEOUT=8         # -t : verify timeout
VERIFY=-1          # -1 auto (cache=skip, fresh=verify), 0 skip (-x), 1 verify (-c)
VERBOSE=0          # -v : print exit IP
FRESH=0            # -r : ignore cache, use a fresh proxy from source
DO_BUILD=0         # -B : build cache and exit

usage() {
  cat >&2 <<'EOF'
SilkRouter (px) — build a proxy cache and route commands through it

usage:
  px.sh [run-opts] <command...>   route a command through a cached proxy
  px.sh -B [build-opts]           (re)build the cache and exit

run options:
  -f FILE   proxy list file            (default $PX_LIST or /tmp/proxy.list)
  -d MAX    max delay ms                (default 1000)
  -D MIN    min delay ms                (default 0)
  -H        http proxies only
  -s        socks proxies only
  -p PROXY  force a specific proxy      (skips list & filters)
  -x        skip verification (default for cache) — fire through first pick
  -c        verify candidates until one answers (default for -r fresh)
  -t SEC    verify timeout              (default 8)
  -r        ignore cache, grab a fresh proxy from the source
  -v        print the exit IP (one request through the chosen proxy)

build options (with -B, or when first-run prompt is accepted):
  -u URL    source proxy-list URL
  -j N      parallel jobs               (default 50)
  -n N      samples averaged per proxy  (default 2)
  -w SEC    per-ping timeout            (default 60)
  -a        also log DEAD proxies

The cache is written as:  protocol://ip:port [Nms]  sorted fastest -> slowest.
EOF
}

# ---- build worker (exported for xargs) ------------------------------------
# emits OK|<avg_ms>|<proxy> or DEAD|<proxy>; persists OK hits to $RES.
test_proxy() {
  local proxy=$1 total=0 ok=0 t i avg
  for ((i=0; i<TRIES; i++)); do
    if t=$(curl -s -o /dev/null --proxy "$proxy" \
             --connect-timeout "$PING_TIMEOUT" --max-time "$PING_TIMEOUT" \
             -w '%{time_total}' "$TEST_URL" 2>/dev/null); then
      total=$(awk -v a="$total" -v b="$t" 'BEGIN{print a+b}')
      ok=$((ok+1))
    fi
  done
  if (( ok > 0 )); then
    avg=$(awk -v tot="$total" -v n="$ok" 'BEGIN{ printf "%d", (tot/n)*1000 }')
    printf '%s\t%s\n' "$avg" "$proxy" >> "$RES"
    printf 'OK|%s|%s\n' "$avg" "$proxy"
  else
    printf 'DEAD|%s\n' "$proxy"
  fi
  return 0
}

# ---- build the cache ------------------------------------------------------
build_cache() {
  local tmp res; tmp=$(mktemp); res=$(mktemp)
  export RES="$res" TRIES PING_TIMEOUT TEST_URL
  export -f test_proxy

  curl -fsSL "$SRC_URL" | tr -d '\r' \
    | grep -E '^(socks5|socks4|https?)://[0-9.]+:[0-9]+$' | sort -u > "$tmp"
  local count; count=$(wc -l < "$tmp")
  if (( count == 0 )); then
    echo "px: no proxies from source" >&2; rm -f "$tmp" "$res"; return 1
  fi

  local G R D Z
  if [[ -t 2 ]]; then G=$'\e[32m' R=$'\e[31m' D=$'\e[2m' Z=$'\e[0m'; else G= R= D= Z=; fi
  echo "px: testing $count proxies (jobs=$JOBS tries=$TRIES, ${PING_TIMEOUT}s/ping) ..." >&2

  xargs -P "$JOBS" -I{} bash -c 'test_proxy "$@"' _ {} < "$tmp" \
  | { i=0; ok=0
      while IFS='|' read -r status f2 f3; do
        i=$((i+1))
        if [[ $status == OK ]]; then
          ok=$((ok+1)); printf '\r\e[2K%s OK %s %s [%sms]\n' "$G" "$Z" "$f3" "$f2" >&2
        elif (( SHOW_ALL )); then
          printf '\r\e[2K%sDEAD%s %s\n' "$R" "$Z" "$f2" >&2
        fi
        printf '\r%stested %d/%d  working %d%s' "$D" "$i" "$count" "$ok" "$Z" >&2
      done
      printf '\n' >&2
    }

  sort -n "$res" | awk -F'\t' '{ printf "%s [%sms]\n", $2, $1 }' > "$LIST"
  local working; working=$(wc -l < "$res")
  echo "px: cached $working/$count working -> $LIST" >&2
  rm -f "$tmp" "$res"
}

# ---- fresh proxies straight from source (untested) ------------------------
fetch_fresh() {
  curl -s "$SRC_URL" | tr -d '\r' \
    | grep -E '^(socks5|socks4|https?)://[0-9.]+:[0-9]+$'
}

# ---- first-run prompt: build a cache? [Y/n] -------------------------------
ask_build() {
  local ans=""
  printf 'px: no cache at %s. Build a tested, sorted cache for best routes? [Y/n] ' "$LIST" >&2
  if ! read -r ans 2>/dev/null < /dev/tty; then read -r ans 2>/dev/null || ans=""; fi
  [[ $ans == [nN]* ]] && return 1
  return 0
}

# ---- run a command through a chosen proxy ---------------------------------
run_mode() {
  (( $# )) || { echo "px: need a command (or -B to build the cache)" >&2; return 2; }

  local -a cand
  if [[ -n $FORCE ]]; then
    cand=("$FORCE")
  else
    if (( ! FRESH )) && [[ ! -r $LIST ]]; then
      if ask_build; then build_cache || return 1; else FRESH=1; fi
    fi
    if (( FRESH )); then
      mapfile -t cand < <(fetch_fresh | shuf)
    else
      mapfile -t cand < <(awk -v lo="$LO" -v hi="$HI" '
        { d=$2; gsub(/[^0-9]/,"",d); if (d=="") d=0
          if (d+0 >= lo && d+0 <= hi) print $1 }' "$LIST" | shuf)
    fi
  fi

  # auto verify policy: fresh proxies are untested -> verify; cache -> trust
  (( VERIFY < 0 )) && { (( FRESH )) && VERIFY=1 || VERIFY=0; }

  # protocol filter
  if [[ $WANT != any ]]; then
    local -a keep=() c pat
    [[ $WANT == http ]] && pat='http://*' || pat='socks*://*'
    for c in "${cand[@]}"; do [[ $c == $pat ]] && keep+=("$c"); done
    cand=("${keep[@]}")
  fi
  (( ${#cand[@]} )) || { echo "px: no proxies match (list=$LIST ${LO}-${HI}ms want=$WANT)" >&2; return 1; }

  # choose
  local chosen="" out="" c
  if [[ -n $FORCE ]]; then
    chosen=$FORCE
  elif (( VERIFY )); then
    for c in "${cand[@]}"; do
      if out=$(curl -x "$c" -m "$VTIMEOUT" -s https://ifconfig.me 2>/dev/null) && [[ -n $out ]]; then
        chosen=$c; break
      fi
    done
  else
    chosen=${cand[0]}
  fi
  [[ -n $chosen ]] || { echo "px: all ${#cand[@]} candidate proxies failed" >&2; return 1; }

  if (( VERBOSE )); then
    [[ -n $out ]] || out=$(curl -x "$chosen" -m "$VTIMEOUT" -s https://ifconfig.me 2>/dev/null)
    [[ -n $out ]] && echo "[px] exit IP via $chosen -> $out" >&2
  fi

  local proto=${chosen%%://*} rest=${chosen#*://}
  local host=${rest%:*} port=${rest##*:} cmd_short=${1##*/}

  ALL_PROXY="$chosen" HTTP_PROXY="$chosen" HTTPS_PROXY="$chosen" \
  all_proxy="$chosen" http_proxy="$chosen" https_proxy="$chosen" \
  "$@" &
  local pid=$!
  echo "[px] proxy: $proto $host:$port  |  cmd: $cmd_short  pid: $pid" >&2
  wait "$pid"
}

# ---- dispatch -------------------------------------------------------------
main() {
  local o OPTIND OPTARG
  while getopts "Brf:u:d:D:Hsp:cxt:j:n:w:avh" o; do
    case "$o" in
      B) DO_BUILD=1 ;;
      r) FRESH=1 ;;
      f) LIST=$OPTARG ;;
      u) SRC_URL=$OPTARG ;;
      d) HI=$OPTARG ;;
      D) LO=$OPTARG ;;
      H) WANT=http ;;
      s) WANT=socks ;;
      p) FORCE=$OPTARG ;;
      c) VERIFY=1 ;;
      x) VERIFY=0 ;;
      t) VTIMEOUT=$OPTARG ;;
      j) JOBS=$OPTARG ;;
      n) TRIES=$OPTARG ;;
      w) PING_TIMEOUT=$OPTARG ;;
      a) SHOW_ALL=1 ;;
      v) VERBOSE=1 ;;
      h) usage; return 0 ;;
      *) usage; return 2 ;;
    esac
  done
  shift $((OPTIND - 1))

  command -v curl >/dev/null || { echo 'px: need curl' >&2; return 1; }

  (( DO_BUILD )) && { build_cache; return $?; }
  run_mode "$@"
}

# run only when executed, not when sourced (so branches stay unit-testable)
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
