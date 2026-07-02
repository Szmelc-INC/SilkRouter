#!/usr/bin/env bash
#
# SilkRouter (px) — build a fast local proxy cache and route commands through it.
#
#   px.sh <command...>     run a command through a random cached proxy
#   px.sh -B               build the cache from the source URL (append-merge)
#   px.sh -R               re-test the proxies already in the cache (prune dead)
#   px.sh -F FILE          build the cache from a local file instead of the URL
#   px.sh -L               print a report of the current cache and exit
#
# On first run (no cache) it offers to build a tested, sorted cache. Decline
# and it grabs a fresh proxy straight from the source instead.
#
set -uo pipefail
export LC_ALL=C            # force '.' decimal in curl's -w, not locale ','

# ---- shared defaults ------------------------------------------------------
# Proxy list (use ~12k list from proxy/LIST.txt by default)
SRC_URL='https://raw.githubusercontent.com/Szmelc-INC/SilkRouter/refs/heads/main/proxy/LIST.txt'
LIST=${PX_LIST:-/tmp/proxy.list}
TEST_URL='http://cp.cloudflare.com/generate_204'   # tiny 204 for delay tests
# build params
JOBS=2000          # parallel workers
TRIES=2            # samples averaged per proxy
PING_TIMEOUT=30    # per-try timeout in seconds (connect + transfer)
SHOW_ALL=0         # -a : log DEAD proxies while building
SRC_FILE=""        # -F : build source is a local file
RECACHE=0          # -R : re-test proxies already in the cache ($LIST)
OVERWRITE=0        # -O : overwrite $LIST instead of append-merging unique hits
DO_LIST=0          # -L : print a report of the cache and exit
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
  px.sh [run-opts] <command...>    route a command through a cached proxy
  px.sh -B [build-opts]            build/append the cache from the source URL
  px.sh -R [build-opts]            re-test the current cache (prunes dead)
  px.sh -F FILE [build-opts]       build the cache from a local file
  px.sh -L | --list                print a report of the current cache and exit

run options:
  -f FILE   proxy list file (cache path)  (default $PX_LIST or /tmp/proxy.list)
  -d MAX    max delay ms                   (default 1000)
  -D MIN    min delay ms                   (default 0)
  -H        http proxies only
  -s        socks proxies only
  -p PROXY  force a specific proxy         (skips list & filters)
  -x        skip verification (default for cache) — fire through first pick
  -c        verify candidates until one answers (default for -r fresh)
  -t SEC    verify timeout                 (default 8)
  -r        ignore cache, grab a fresh proxy from the source
  -v        print the exit IP (one request through the chosen proxy)

build options (with -B / -R / -F, or when the first-run prompt is accepted):
  -u URL    source proxy-list URL
  -F FILE   build from a local proxy-list file instead of the URL
  -R        re-test proxies already in the cache ($PX_LIST); prunes dead ones
  -O        overwrite the cache instead of append-merging unique entries
  -j N      parallel jobs                  (default 2000)
  -n N      tries averaged per proxy       (default 2)
  -w SEC    per-try timeout in seconds     (default 30)
  -a        also log DEAD proxies

Long options: --list, --recache, --file[=FILE], --url[=URL], --overwrite,
              --build, --fresh, --help

By default -B/-F APPEND unique proxies to the cache (new timings win on
duplicates) and re-sort fastest -> slowest. -R and -O OVERWRITE instead.
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

# ---- gather raw proxy candidates from the chosen source -------------------
# recache -> current cache ($LIST) ; -F FILE -> local file ; else -> URL.
gather_source() {
  if (( RECACHE )); then
    if [[ ! -r $LIST ]]; then
      echo "px: no cache to re-test at $LIST" >&2; return 1
    fi
    awk '{print $1}' "$LIST"          # strip the [Nms] suffix
  elif [[ -n $SRC_FILE ]]; then
    if [[ ! -r $SRC_FILE ]]; then
      echo "px: cannot read source file $SRC_FILE" >&2; return 1
    fi
    cat "$SRC_FILE"
  else
    curl -fsSL "$SRC_URL"
  fi
}

# ---- merge new results into $LIST (append unique, new timings win) --------
# arg1 = results file with lines "<ms>\t<proxy>". Existing + new are deduped
# by proxy, new measurements override old, output re-sorted fastest->slowest.
merge_into_list() {
  local newres=$1 out; out=$(mktemp)
  {
    # existing cache lines "proxy [Nms]" -> "ms\tproxy"
    [[ -r $LIST ]] && awk '{ p=$1; d=$2; gsub(/[^0-9]/,"",d); if(d=="")d=0; print d"\t"p }' "$LIST"
    # fresh results are already "ms\tproxy"; appended last so they win on dupes
    cat "$newres"
  } | awk -F'\t' '{ d[$2]=$1 } END { for (p in d) print d[p]"\t"p }' \
    | sort -n | awk -F'\t' '{ printf "%s [%sms]\n", $2, $1 }' > "$out"
  mv "$out" "$LIST"
}

# ---- build the cache ------------------------------------------------------
build_cache() {
  local tmp res; tmp=$(mktemp); res=$(mktemp)
  export RES="$res" TRIES PING_TIMEOUT TEST_URL
  export -f test_proxy

  # write policy: re-cache refreshes the same file -> prune dead (overwrite)
  local overwrite=$OVERWRITE
  (( RECACHE )) && overwrite=1

  gather_source | tr -d '\r' \
    | grep -E '^(socks5|socks4|https?)://[0-9.]+:[0-9]+$' | sort -u > "$tmp"
  local count; count=$(wc -l < "$tmp")
  if (( count == 0 )); then
    echo "px: no proxies from source" >&2; rm -f "$tmp" "$res"; return 1
  fi

  local G R D Z
  if [[ -t 2 ]]; then G=$'\e[32m' R=$'\e[31m' D=$'\e[2m' Z=$'\e[0m'; else G= R= D= Z=; fi
  local srcdesc
  if   (( RECACHE ));      then srcdesc="re-cache $LIST"
  elif [[ -n $SRC_FILE ]]; then srcdesc="file $SRC_FILE"
  else                          srcdesc="url"; fi
  echo "px: testing $count proxies from $srcdesc (jobs=$JOBS tries=$TRIES, ${PING_TIMEOUT}s/try) ..." >&2

  # The pipeline below blocks until every xargs worker has either returned or
  # hit its ${PING_TIMEOUT}s ceiling (max TRIES*PING_TIMEOUT per proxy), so the
  # last tests are fully collected — nothing is dropped on the final batch.
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

  local working; working=$(wc -l < "$res" 2>/dev/null || echo 0)
  if (( working == 0 )); then
    echo "px: 0 working proxies this run" >&2
    (( overwrite )) && [[ -r $LIST ]] && \
      echo "px: keeping existing cache (refusing to overwrite with an empty result)" >&2
    rm -f "$tmp" "$res"; return 1
  fi

  if (( overwrite )); then
    sort -n "$res" | awk -F'\t' '{ printf "%s [%sms]\n", $2, $1 }' > "$LIST"
    echo "px: wrote $working working -> $LIST (overwrite)" >&2
  else
    merge_into_list "$res"
    echo "px: merged $working new into $LIST -> $(wc -l < "$LIST") total (deduped)" >&2
  fi
  rm -f "$tmp" "$res"
}

# ---- report on the current cache ------------------------------------------
list_report() {
  if [[ ! -r $LIST ]]; then echo "px: no cache at $LIST" >&2; return 1; fi
  local when; when=$(date -r "$LIST" +%d/%m/%Y 2>/dev/null || date +%d/%m/%Y)
  awk -v when="$when" '
    {
      total++
      proto=$1; sub(/:\/\/.*/,"",proto); pc[proto]++
      d=$2; gsub(/[^0-9]/,"",d); if(d=="")d=0; d+=0
      if (d < 100)       b100++
      if (d <= 1000)     s01++
      else if (d <= 5000)  s15++
      else if (d <= 10000) s510++
      else                 s10++
    }
    END {
      printf "# List cached at `%s`\n", when
      print "Proxies"
      printf "* total: [%d]\n", total
      order="http https socks4 socks5"; n=split(order, ord, " ")
      for (i=1;i<=n;i++){ p=ord[i]; if (p in pc){ printf "* %s: [%d]\n", p, pc[p]; seen[p]=1 } }
      for (p in pc){ if (!(p in seen)) printf "* %s: [%d]\n", p, pc[p] }
      print ""
      print "Delay"
      printf "* Below 100ms: [%d]\n", b100+0
      printf "* 0-1s: [%d]\n",  s01+0
      printf "* 1-5s: [%d]\n",  s15+0
      printf "* 5-10s: [%d]\n", s510+0
      printf "* 10s+: [%d]\n",  s10+0
    }
  ' "$LIST"
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
  # normalize long options -> short equivalents before getopts
  local a; local -a norm=()
  for a in "$@"; do
    case "$a" in
      --list)      norm+=(-L) ;;
      --recache)   norm+=(-R) ;;
      --overwrite) norm+=(-O) ;;
      --build)     norm+=(-B) ;;
      --fresh)     norm+=(-r) ;;
      --help)      norm+=(-h) ;;
      --file=*)    norm+=(-F "${a#*=}") ;;
      --file)      norm+=(-F) ;;
      --url=*)     norm+=(-u "${a#*=}") ;;
      --url)       norm+=(-u) ;;
      *)           norm+=("$a") ;;
    esac
  done
  set -- "${norm[@]}"

  local o OPTIND OPTARG
  while getopts "BRLOrf:F:u:d:D:Hsp:cxt:j:n:w:avh" o; do
    case "$o" in
      B) DO_BUILD=1 ;;
      R) RECACHE=1 ;;
      L) DO_LIST=1 ;;
      O) OVERWRITE=1 ;;
      r) FRESH=1 ;;
      f) LIST=$OPTARG ;;
      F) SRC_FILE=$OPTARG ;;
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

  (( DO_LIST )) && { list_report; return $?; }
  if (( DO_BUILD || RECACHE )) || [[ -n $SRC_FILE ]]; then
    build_cache; return $?
  fi
  run_mode "$@"
}

# run only when executed, not when sourced (so branches stay unit-testable)
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
