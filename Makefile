# SilkRouter - Makefile
# Build, test, install and package the CGO-free Go binary.
# Run `make` (or `make help`) for the list of targets.

BINARY      := silkrouter
# Other names of the same binary (installed as symlinks).
ALIASES     := px s-proxy
PKG         := .
DIST        := dist
SHELLS      := zsh bash fish

# Install locations. The default needs no root: $(HOME)/.local/bin is on
# most PATHs. Packagers use PREFIX=/usr plus DESTDIR=<staging dir>.
PREFIX       ?= $(HOME)/.local
DESTDIR      ?=
BINDIR       ?= $(PREFIX)/bin
DATADIR      ?= $(PREFIX)/share
DOCDIR       ?= $(DATADIR)/doc/$(BINARY)
SHAREDIR     ?= $(DATADIR)/$(BINARY)
ZSH_COMPDIR  ?= $(DATADIR)/zsh/site-functions
BASH_COMPDIR ?= $(DATADIR)/bash-completion/completions
FISH_COMPDIR ?= $(DATADIR)/fish/vendor_completions.d

# Generated files (gitignored).
COMPDIR     := completions
GENDIR      := build

# Version is derived from git when available, else a sane default.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 4.0.0-dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS_BUILD ?= -trimpath
GO      ?= go

# CGO off => fully static, portable binaries.
export CGO_ENABLED := 0

# Cross-compile matrix: GOOS/GOARCH pairs shipped by `make release`.
PLATFORMS := \
	linux/amd64 linux/arm64 linux/arm/7 linux/386 \
	windows/amd64 windows/arm64 \
	darwin/amd64 darwin/arm64 \
	freebsd/amd64

# Colors for help / messages (NO_COLOR or a dumb terminal disables them).
ifeq ($(NO_COLOR)$(filter dumb,$(TERM)),)
  C_RESET := \033[0m
  C_BOLD  := \033[1m
  C_DIM   := \033[2m
  C_CYAN  := \033[36m
  C_GREEN := \033[32m
  C_YEL   := \033[33m
  C_MAG   := \033[35m
else
  C_RESET :=
  C_BOLD  :=
  C_DIM   :=
  C_CYAN  :=
  C_GREEN :=
  C_YEL   :=
  C_MAG   :=
endif

.DEFAULT_GOAL := help

##@ Help

## help: show this help (the default target)
.PHONY: help
help:
	@printf "$(C_BOLD)$(C_MAG)SilkRouter$(C_RESET) $(C_DIM)$(VERSION)$(C_RESET) - route any command through fast, scanned, ranked proxies\n"
	@printf "$(C_DIM)binary $(BINARY) (+ $(ALIASES)), PREFIX=$(PREFIX)$(C_RESET)\n\n"
	@printf "$(C_BOLD)Usage:$(C_RESET) make $(C_CYAN)<target>$(C_RESET) [VAR=value ...]\n"
	@awk 'BEGIN { FS = ":" } \
		/^##@/ { printf "\n$(C_BOLD)$(C_YEL)%s$(C_RESET)\n", substr($$0, 5); next } \
		/^## [a-zA-Z0-9_.-]+:/ { name = substr($$1, 4); sub(/^[^:]*: */, "", $$0); \
			printf "  $(C_CYAN)%-16s$(C_RESET) %s\n", name, $$0 }' $(MAKEFILE_LIST)
	@printf "\n$(C_BOLD)Variables:$(C_RESET)\n"
	@printf "  $(C_GREEN)%-16s$(C_RESET) %s\n" PREFIX "install prefix ($(PREFIX); packagers: /usr)" \
		DESTDIR "staging root for packages" VERSION "version stamped into the binary ($(VERSION))" \
		ARGS "arguments for run / tui (make run ARGS='scan quick')" NO_COLOR "disable colors in this help"
	@printf "\n$(C_DIM)Examples: make build | make check | make install | make install PREFIX=/usr DESTDIR=pkg | make release$(C_RESET)\n"

##@ Build

## build: build ./silkrouter for this machine
.PHONY: build
build:
	$(GO) build $(GOFLAGS_BUILD) -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)
	@printf "$(C_GREEN)built$(C_RESET) ./$(BINARY) ($(VERSION))\n"

## run: build and run (make run ARGS="proxies --status alive")
.PHONY: run
run: build
	./$(BINARY) $(ARGS)

## tui: build and start the interactive TUI
.PHONY: tui
tui: build
	./$(BINARY) tui $(ARGS)

## completions: generate zsh/bash/fish completion scripts into completions/
.PHONY: completions
completions: build
	@mkdir -p $(COMPDIR)
	./$(BINARY) completion zsh  > $(COMPDIR)/_$(BINARY)
	./$(BINARY) completion bash > $(COMPDIR)/$(BINARY).bash
	./$(BINARY) completion fish > $(COMPDIR)/$(BINARY).fish

## shell-snippets: generate the shell integration scripts into build/shell/
.PHONY: shell-snippets
shell-snippets: build
	@mkdir -p $(GENDIR)/shell
	@for sh in $(SHELLS); do \
		./$(BINARY) shell-init $$sh --bin $(BINDIR)/$(BINARY) > $(GENDIR)/shell/init.$$sh || exit 1; \
	done
	@printf "$(C_GREEN)wrote$(C_RESET) $(GENDIR)/shell/init.{$(shell echo $(SHELLS) | tr ' ' ',')}\n"

## config-example: write the commented default config to build/config.toml.example
.PHONY: config-example
config-example: build
	@mkdir -p $(GENDIR)/home
	@SILKROUTER_HOME=$(CURDIR)/$(GENDIR)/home SILKROUTER_CONFIG= ./$(BINARY) -q config init --force >/dev/null 2>&1 || \
		SILKROUTER_HOME=$(CURDIR)/$(GENDIR)/home SILKROUTER_CONFIG= ./$(BINARY) -q config init >/dev/null
	@cp $(GENDIR)/home/config.toml $(GENDIR)/config.toml.example
	@printf "$(C_GREEN)wrote$(C_RESET) $(GENDIR)/config.toml.example\n"

## release: cross-compile for every platform into dist/ (+ SHA256SUMS)
.PHONY: release
release: clean-dist
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$$(echo $$platform | cut -d/ -f1); arch=$$(echo $$platform | cut -d/ -f2); arm=$$(echo $$platform | cut -d/ -f3); \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		suffix="$$os-$$arch"; [ -n "$$arm" ] && suffix="$$suffix""v$$arm"; \
		out="$(DIST)/$(BINARY)-$$suffix$$ext"; \
		printf "building $(C_CYAN)%s$(C_RESET)\n" "$$out"; \
		GOOS=$$os GOARCH=$$arch GOARM=$$arm $(GO) build $(GOFLAGS_BUILD) -ldflags "$(LDFLAGS)" -o "$$out" $(PKG) || exit 1; \
	done
	@cd $(DIST) && (sha256sum * > SHA256SUMS 2>/dev/null || shasum -a 256 * > SHA256SUMS)
	@printf "$(C_GREEN)release binaries$(C_RESET) in $(DIST)/ (SHA256SUMS included)\n"

## dist: source tarball of HEAD into dist/
.PHONY: dist
dist:
	@mkdir -p $(DIST)
	git archive --format=tar.gz --prefix=$(BINARY)-$(VERSION)/ -o $(DIST)/$(BINARY)-$(VERSION).tar.gz HEAD
	@printf "$(C_GREEN)wrote$(C_RESET) $(DIST)/$(BINARY)-$(VERSION).tar.gz\n"

##@ Install

## install: install binary, px/s-proxy links, completions, shell snippets, docs (no root needed)
.PHONY: install
install: build completions shell-snippets config-example
	install -d $(DESTDIR)$(BINDIR) $(DESTDIR)$(ZSH_COMPDIR) $(DESTDIR)$(BASH_COMPDIR) $(DESTDIR)$(FISH_COMPDIR) \
		$(DESTDIR)$(SHAREDIR)/shell $(DESTDIR)$(DOCDIR)
	install -m 0755 $(BINARY) $(DESTDIR)$(BINDIR)/$(BINARY)
	for a in $(ALIASES); do ln -sf $(BINARY) $(DESTDIR)$(BINDIR)/$$a; done
	install -m 0644 $(COMPDIR)/_$(BINARY) $(DESTDIR)$(ZSH_COMPDIR)/_$(BINARY)
	install -m 0644 $(COMPDIR)/$(BINARY).bash $(DESTDIR)$(BASH_COMPDIR)/$(BINARY)
	for a in $(ALIASES); do ln -sf $(BINARY) $(DESTDIR)$(BASH_COMPDIR)/$$a; done
	install -m 0644 $(COMPDIR)/$(BINARY).fish $(DESTDIR)$(FISH_COMPDIR)/$(BINARY).fish
	for a in $(ALIASES); do ln -sf $(BINARY).fish $(DESTDIR)$(FISH_COMPDIR)/$$a.fish; done
	for sh in $(SHELLS); do install -m 0644 $(GENDIR)/shell/init.$$sh $(DESTDIR)$(SHAREDIR)/shell/init.$$sh; done
	install -m 0644 $(GENDIR)/config.toml.example $(DESTDIR)$(SHAREDIR)/config.toml.example
	install -m 0644 README.md docs/ARCHITECTURE-v4.md $(DESTDIR)$(DOCDIR)/
	@[ ! -f docs/TUI.md ] || install -m 0644 docs/TUI.md $(DESTDIR)$(DOCDIR)/
	@printf "$(C_GREEN)installed$(C_RESET) $(BINARY), $(ALIASES) into $(DESTDIR)$(BINDIR); completions, shell snippets and docs under $(DESTDIR)$(DATADIR)\n"
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; *) [ -n "$(DESTDIR)" ] || printf "$(C_YEL)note:$(C_RESET) $(BINDIR) is not on your PATH\n";; esac
	@[ -n "$(DESTDIR)" ] || printf "next: $(C_CYAN)$(BINARY) shell install$(C_RESET) (shell integration), then $(C_CYAN)px scan full$(C_RESET)\n"

## uninstall: remove everything install put under PREFIX
.PHONY: uninstall
uninstall:
	rm -f $(DESTDIR)$(BINDIR)/$(BINARY) $(DESTDIR)$(ZSH_COMPDIR)/_$(BINARY)
	rm -f $(DESTDIR)$(BASH_COMPDIR)/$(BINARY) $(DESTDIR)$(FISH_COMPDIR)/$(BINARY).fish
	for a in $(ALIASES); do rm -f $(DESTDIR)$(BINDIR)/$$a $(DESTDIR)$(BASH_COMPDIR)/$$a $(DESTDIR)$(FISH_COMPDIR)/$$a.fish; done
	rm -rf $(DESTDIR)$(SHAREDIR) $(DESTDIR)$(DOCDIR)

## shell-install: add the shell integration to your rc files (silkrouter shell install)
.PHONY: shell-install
shell-install: build
	./$(BINARY) shell install $(ARGS)

##@ Develop

## check: fmt-check + vet + test (run before committing)
.PHONY: check
check: fmt-check vet test

## test: run the test suite
.PHONY: test
test:
	$(GO) test ./...

## test-v: run the test suite verbosely
.PHONY: test-v
test-v:
	$(GO) test -v ./...

## test-race: run the test suite with the race detector (needs a C toolchain)
.PHONY: test-race
test-race:
	CGO_ENABLED=1 $(GO) test -race ./...

## cover: test with coverage, write build/coverage.html
.PHONY: cover
cover:
	@mkdir -p $(GENDIR)
	$(GO) test -coverprofile=$(GENDIR)/coverage.out ./...
	$(GO) tool cover -html=$(GENDIR)/coverage.out -o $(GENDIR)/coverage.html
	@$(GO) tool cover -func=$(GENDIR)/coverage.out | tail -1

## vet: run go vet
.PHONY: vet
vet:
	$(GO) vet ./...

## lint: run golangci-lint when installed (else go vet)
.PHONY: lint
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else printf "$(C_YEL)golangci-lint not installed$(C_RESET), running go vet\n"; $(GO) vet ./...; fi

## fmt: format all Go sources
.PHONY: fmt
fmt:
	gofmt -w .

## fmt-check: fail if any file is unformatted (CI-friendly)
.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

## tidy: sync go.mod/go.sum
.PHONY: tidy
tidy:
	$(GO) mod tidy

## deps: download module dependencies
.PHONY: deps
deps:
	$(GO) mod download

## update: pull the latest source and rebuild
.PHONY: update
update:
	git pull --ff-only
	$(MAKE) build

##@ Package

## pkg-arch: build the Arch package with makepkg (packaging/arch/PKGBUILD)
.PHONY: pkg-arch
pkg-arch:
	cd packaging/arch && makepkg -f $(ARGS)

## srcinfo: regenerate packaging/arch/.SRCINFO
.PHONY: srcinfo
srcinfo:
	cd packaging/arch && makepkg --printsrcinfo > .SRCINFO

## nix-build: build the flake package (result/)
.PHONY: nix-build
nix-build:
	nix build .#default -L

## nix-run: run the flake app (make nix-run ARGS="--version")
.PHONY: nix-run
nix-run:
	nix run .#default -- $(ARGS)

## nix-shell: enter the flake dev shell
.PHONY: nix-shell
nix-shell:
	nix develop

## nix-hash: print the vendorHash nix expects (after go.mod changes)
.PHONY: nix-hash
nix-hash:
	@nix build .#default 2>&1 | sed -n 's/.*got: *//p' | head -1

##@ Clean

## clean: remove built artifacts
.PHONY: clean
clean: clean-dist
	rm -f $(BINARY)
	rm -rf $(COMPDIR) $(GENDIR)

## clean-dist: remove dist/
.PHONY: clean-dist
clean-dist:
	rm -rf $(DIST)
