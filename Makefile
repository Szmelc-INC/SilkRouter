# SilkRouter - Makefile
# Build, test and cross-compile the dependency-free Go binary.

BINARY      := silkrouter
PKG         := .
DIST        := dist
PREFIX      ?= /usr/local
INSTALL_DIR := $(PREFIX)/bin

# Version is derived from git when available, else a sane default.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 2.0.0)
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

## install: install the binary into $(INSTALL_DIR)
.PHONY: install
install: build
	install -d $(INSTALL_DIR)
	install -m 0755 $(BINARY) $(INSTALL_DIR)/$(BINARY)
	@echo "installed $(INSTALL_DIR)/$(BINARY)"

## uninstall: remove the installed binary
.PHONY: uninstall
uninstall:
	rm -f $(INSTALL_DIR)/$(BINARY)

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

.PHONY: clean-dist
clean-dist:
	rm -rf $(DIST)
