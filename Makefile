# SilkRouter - Makefile
# Build, test and cross-compile the dependency-free Go binary.

BINARY      := silkrouter
# Other names of the same binary (installed as symlinks).
ALIASES     := px s-proxy
PKG         := .
DIST        := dist

# Install locations. The default needs no root: $(HOME)/.local/bin is on
# most PATHs. Packagers use PREFIX=/usr plus DESTDIR=<staging dir>.
PREFIX       ?= $(HOME)/.local
DESTDIR      ?=
BINDIR       ?= $(PREFIX)/bin
DATADIR      ?= $(PREFIX)/share
ZSH_COMPDIR  ?= $(DATADIR)/zsh/site-functions
BASH_COMPDIR ?= $(DATADIR)/bash-completion/completions
FISH_COMPDIR ?= $(DATADIR)/fish/vendor_completions.d

# Generated shell completions (gitignored; `make completions` rebuilds them).
COMPDIR     := completions

# Version is derived from git when available, else a sane default.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 4.0.0-dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# CGO off => fully static, portable binaries.
export CGO_ENABLED := 0

# Cross-compile matrix: GOOS/GOARCH pairs shipped by `make release`.
PLATFORMS := \
	linux/amd64 linux/arm64 \
	windows/amd64 windows/arm64 \
	darwin/amd64 darwin/arm64

.DEFAULT_GOAL := build

## help: list available targets
.PHONY: help
help:
	@echo "SilkRouter make targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'

## build: build the binary for the host platform
.PHONY: build
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)
	@echo "built ./$(BINARY) ($(VERSION))"

## run: build and run (pass args with ARGS="...")
.PHONY: run
run:
	go run $(PKG) $(ARGS)

## completions: generate zsh/bash/fish completion scripts into completions/
.PHONY: completions
completions: build
	@mkdir -p $(COMPDIR)
	./$(BINARY) completion zsh  > $(COMPDIR)/_$(BINARY)
	./$(BINARY) completion bash > $(COMPDIR)/$(BINARY).bash
	./$(BINARY) completion fish > $(COMPDIR)/$(BINARY).fish

## install: install silkrouter, px/s-proxy symlinks and completions under $(PREFIX) (no root needed)
.PHONY: install
install: build completions
	install -d $(DESTDIR)$(BINDIR) $(DESTDIR)$(ZSH_COMPDIR) $(DESTDIR)$(BASH_COMPDIR) $(DESTDIR)$(FISH_COMPDIR)
	install -m 0755 $(BINARY) $(DESTDIR)$(BINDIR)/$(BINARY)
	for a in $(ALIASES); do ln -sf $(BINARY) $(DESTDIR)$(BINDIR)/$$a; done
	install -m 0644 $(COMPDIR)/_$(BINARY) $(DESTDIR)$(ZSH_COMPDIR)/_$(BINARY)
	install -m 0644 $(COMPDIR)/$(BINARY).bash $(DESTDIR)$(BASH_COMPDIR)/$(BINARY)
	for a in $(ALIASES); do ln -sf $(BINARY) $(DESTDIR)$(BASH_COMPDIR)/$$a; done
	install -m 0644 $(COMPDIR)/$(BINARY).fish $(DESTDIR)$(FISH_COMPDIR)/$(BINARY).fish
	for a in $(ALIASES); do ln -sf $(BINARY).fish $(DESTDIR)$(FISH_COMPDIR)/$$a.fish; done
	@echo "installed $(BINARY), $(ALIASES) into $(DESTDIR)$(BINDIR) and completions under $(DESTDIR)$(DATADIR)"
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; *) echo "note: $(BINDIR) is not on your PATH";; esac

## uninstall: remove everything install put under $(PREFIX)
.PHONY: uninstall
uninstall:
	rm -f $(DESTDIR)$(BINDIR)/$(BINARY) $(DESTDIR)$(ZSH_COMPDIR)/_$(BINARY)
	rm -f $(DESTDIR)$(BASH_COMPDIR)/$(BINARY) $(DESTDIR)$(FISH_COMPDIR)/$(BINARY).fish
	for a in $(ALIASES); do rm -f $(DESTDIR)$(BINDIR)/$$a $(DESTDIR)$(BASH_COMPDIR)/$$a $(DESTDIR)$(FISH_COMPDIR)/$$a.fish; done

## test: run the test suite
.PHONY: test
test:
	go test ./...

## test-race: run the test suite with the race detector (needs a C toolchain)
.PHONY: test-race
test-race:
	CGO_ENABLED=1 go test -race ./...

## vet: run go vet
.PHONY: vet
vet:
	go vet ./...

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
	go mod tidy

## check: fmt-check + vet + test (run before committing)
.PHONY: check
check: fmt-check vet test

## release: cross-compile for every platform into $(DIST)/
.PHONY: release
release: clean-dist
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		out="$(DIST)/$(BINARY)-$$os-$$arch$$ext"; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o "$$out" $(PKG) || exit 1; \
	done
	@echo "release binaries in $(DIST)/"

## update: pull latest source and rebuild
.PHONY: update
update:
	git pull --ff-only
	$(MAKE) build

## clean: remove built artifacts
.PHONY: clean
clean: clean-dist
	rm -f $(BINARY)
	rm -rf $(COMPDIR)

.PHONY: clean-dist
clean-dist:
	rm -rf $(DIST)
