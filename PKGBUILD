# Maintainer: SilverX <serainox@gmail.com>
pkgname=silkrouter-git
_repo=SilkRouter
pkgver=r0.0000000
pkgrel=1
pkgdesc="Route any command or shell through fast, scanned and ranked proxies (px / s-proxy / silkrouter)"
arch=('x86_64' 'aarch64')
url="https://github.com/Szmelc-INC/SilkRouter"
license=('MIT')
depends=('sqlite')
makedepends=('git' 'go' 'make')
optdepends=('jq: JSON inspection of the proxy store')
provides=('silkrouter' 'px' 's-proxy')
conflicts=('silkrouter' 'px' 's-proxy')
options=('!debug' '!strip')
source=("git+$url.git")
sha256sums=('SKIP')

pkgver() {
  cd "$_repo"
  ( set -o pipefail
    git describe --long --tags --abbrev=7 2>/dev/null | sed 's/^v//;s/\([^-]*-g\)/r\1/;s/-/./g' ||
    printf "r%s.%s" "$(git rev-list --count HEAD)" "$(git rev-parse --short=7 HEAD)" )
}

prepare() {
  cd "$_repo"
  export GOPATH="$srcdir/gopath" GOFLAGS="-modcacherw"
  go mod download
}

build() {
  cd "$_repo"
  export GOPATH="$srcdir/gopath" CGO_ENABLED=0 GOFLAGS="-trimpath -mod=readonly -modcacherw"
  make build BINARY=silkrouter
  make completions BINARY=silkrouter
}

check() {
  cd "$_repo"
  export GOPATH="$srcdir/gopath" CGO_ENABLED=0 GOFLAGS="-mod=readonly -modcacherw"
  make test
}

package() {
  cd "$_repo"
  install -Dm755 silkrouter "$pkgdir/usr/bin/silkrouter"
  ln -sf silkrouter "$pkgdir/usr/bin/px"
  ln -sf silkrouter "$pkgdir/usr/bin/s-proxy"
  install -Dm644 completions/_silkrouter "$pkgdir/usr/share/zsh/site-functions/_silkrouter"
  install -Dm644 completions/silkrouter.bash "$pkgdir/usr/share/bash-completion/completions/silkrouter"
  install -Dm644 completions/silkrouter.fish "$pkgdir/usr/share/fish/vendor_completions.d/silkrouter.fish"
  install -Dm644 README.md -t "$pkgdir/usr/share/doc/$pkgname/"
  install -Dm644 LICENSE "$pkgdir/usr/share/licenses/$pkgname/LICENSE"
}
