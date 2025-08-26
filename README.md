# SilkRouter
Integrated proxy in shell, to route anything through.

## Use Example:
```bash
px -v curl https://ifconfig.me
px -H wget -qO- https://szmelc.com
px -H firefox
```

### Code
> Prefferably append to .bashrc / .zshrc (mind, you have to source it each time to get new IP)
```bash
# --- Random proxy fetcher + runner (retry until works) -----------------------
getp() {
  local url='https://api.proxyscrape.com/v4/free-proxy-list/get?request=display_proxies&proxy_format=protocolipport&format=text&timeout=20000'
  curl -s "$url" | tr -d '\r' \
    | awk '/^http:\/\// || /^socks4:\/\// {print}'
}

# px: run a command via a working proxy
# usage: px [options] <command...>
#   -p proxy     use specific proxy
#   -H           prefer http proxies
#   -s           prefer socks4 proxies
#   -t sec       test timeout (default 8)
#   -v           verbose (print chosen proxy+IP)
px() {
  local p="" m=8 v=0 want="any"
  while getopts "p:t:vHs" o; do
    case "$o" in
      p) p="$OPTARG" ;;
      t) m="$OPTARG" ;;
      v) v=1 ;;
      H) want="http" ;;
      s) want="socks4" ;;
    esac
  done
  shift $((OPTIND - 1))
  (( $# == 0 )) && { echo "px: need a command" >&2; return 2; }

  # pull fresh proxy list
  local list=($(getp))
  [[ ${#list[@]} -eq 0 ]] && { echo "px: no proxies from source" >&2; return 1; }

  # shuffle order
  list=($(printf "%s\n" "${list[@]}" | shuf))

  # try until something works
  local ok=0
  for candidate in "${list[@]}"; do
    case "$want" in
      http)   [[ "$candidate" == http://* ]]    || continue ;;
      socks4) [[ "$candidate" == socks4://* ]] || continue ;;
    esac
    if out=$(curl -x "$candidate" -m "$m" -s https://ifconfig.me); then
      p="$candidate"
      ok=1
      (( v )) && echo "[px] using $p -> $out"
      break
    fi
  done

  [[ $ok -eq 0 ]] && { echo "px: all proxies failed." >&2; return 1; }

  # run the command through it (env vars way)
  ALL_PROXY="$p" HTTP_PROXY="$p" HTTPS_PROXY="$p" \
  all_proxy="$p" http_proxy="$p" https_proxy="$p" \
  "$@"
}
# ----------------------------------------------------------------------------- 

```
