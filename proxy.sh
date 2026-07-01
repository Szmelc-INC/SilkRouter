# --- Cached-proxy picker + runner -------------------------------------------
# Fallback source, only used when the cache is missing or -r is given.
getp() {
  local url='https://api.proxyscrape.com/v4/free-proxy-list/get?request=display_proxies&proxy_format=protocolipport&format=text&timeout=20000'
  curl -s "$url" | tr -d '\r' | awk '/^(https?|socks[45]):\/\// {print $1}'
}

# px: run a command through a working proxy picked at random from the local
#     cached list (default /tmp/proxy.list, lines: "proto://ip:port [Nms]").
#
# usage: px [opts] <command...>
#   -f FILE   proxy list file          (default $PX_LIST or /tmp/proxy.list)
#   -d MAX    max delay ms              (default 1000)
#   -D MIN    min delay ms              (default 0)
#   -p PROXY  force a specific proxy    (skips the list & delay filter)
#   -H        http proxies only
#   -s        socks proxies only
#   -t SEC    verify timeout            (default 8)
#   -r        ignore cache, fetch fresh from source
#   -v        also print the exit IP
px() {
  emulate -L zsh
  setopt local_options no_monitor
  local file=${PX_LIST:-/tmp/proxy.list}
  local p="" m=8 v=0 want=any lo=0 hi=1000 fresh=0
  local OPTIND OPTARG o
  while getopts "f:d:D:p:t:Hsrv" o; do
    case $o in
      f) file=$OPTARG ;;
      d) hi=$OPTARG ;;
      D) lo=$OPTARG ;;
      p) p=$OPTARG ;;
      t) m=$OPTARG ;;
      H) want=http ;;
      s) want=socks ;;
      r) fresh=1 ;;
      v) v=1 ;;
    esac
  done
  shift $((OPTIND - 1))
  (( $# )) || { print -u2 "px: need a command"; return 2 }

  # build candidate list, in random order
  local -a cand
  if [[ -n $p ]]; then
    cand=($p)
  elif (( fresh )) || [[ ! -r $file ]]; then
    (( fresh )) || print -u2 "px: no cache at $file — fetching fresh list"
    cand=(${(f)"$(getp | shuf)"})
  else
    cand=(${(f)"$(
      awk -v lo=$lo -v hi=$hi '
        { d=$2; gsub(/[^0-9]/,"",d); if (d=="") d=0
          if (d+0 >= lo && d+0 <= hi) print $1 }' "$file" | shuf
    )"})
  fi
  cand=("${(@)cand:#}")                       # drop any empty elements

  # protocol filter
  [[ $want == http  ]] && cand=(${(M)cand:#http://*})
  [[ $want == socks ]] && cand=(${(M)cand:#socks*})
  (( ${#cand} )) || { print -u2 "px: no proxies match (file=$file ${lo}-${hi}ms want=$want)"; return 1 }

  # pick the first that actually works (random order => random working proxy)
  local chosen="" out c
  if [[ -n $p ]]; then
    chosen=$p
  else
    for c in $cand; do
      if out=$(curl -x "$c" -m "$m" -s https://ifconfig.me 2>/dev/null) && [[ -n $out ]]; then
        chosen=$c
        (( v )) && print -u2 "[px] exit IP via $c -> $out"
        break
      fi
    done
  fi
  [[ -n $chosen ]] || { print -u2 "px: all ${#cand} candidate proxies failed"; return 1 }

  # split proto://ip:port
  local proto=${chosen%%://*} rest=${chosen#*://}
  local host=${rest%:*} port=${rest##*:}
  local cmd_short=${1:t}

  # run the command through it, then report proxy + command + PID
  ALL_PROXY=$chosen HTTP_PROXY=$chosen HTTPS_PROXY=$chosen \
  all_proxy=$chosen http_proxy=$chosen https_proxy=$chosen \
  "$@" &
  local pid=$!
  print -u2 -- "[px] proxy: ${proto} ${host}:${port}  |  cmd: ${cmd_short}  pid: ${pid}"
  wait $pid
}
# ----------------------------------------------------------------------------
