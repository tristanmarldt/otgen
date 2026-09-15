SHELL := /bin/sh

BINDIR ?= $(HOME)/.local/bin
BINARY := $(CURDIR)/otgen
PATH_LINK := $(BINDIR)/otgen

.DEFAULT_GOAL := install

.PHONY: build install test verify

# The PATH entry is a stable link to the repository build. That means both this
# target and a later plain `go build` immediately update the `otgen` command.
# Build to a temporary file first so failure keeps the last working executable.
build install:
	@mkdir -p "$(BINDIR)"
	@tmp="$$(mktemp "$(CURDIR)/.otgen.XXXXXX")"; \
		trap 'rm -f "$$tmp"' EXIT INT TERM; \
		go build -o "$$tmp" .; \
		chmod 755 "$$tmp"; \
		mv -f "$$tmp" "$(BINARY)"; \
		trap - EXIT INT TERM
	@ln -sfn "$(BINARY)" "$(PATH_LINK)"
	@printf 'Built %s and linked %s\n' "$(BINARY)" "$(PATH_LINK)"

test:
	go test ./...

verify:
	go test ./...
	go vet ./...
