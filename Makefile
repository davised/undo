PREFIX ?= $(HOME)/.local
CC ?= gcc
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

GO_SRC := $(shell find cmd internal -name '*.go')

all: hooks bin/undo build/libundo.so

# Git cannot version-control hooks directly; core.hooksPath is the supported
# indirection and it is per-clone, so it is unset again after every fresh
# clone -- silently, which is the worst way for a safety check to be missing.
# Making it a prerequisite of the default target means anyone who builds gets
# it without having to know it exists.
#
# Guarded so it is a no-op outside a work tree: the test container copies the
# source without .git, and a build must not fail there.
hooks:
	@if git rev-parse --git-dir >/dev/null 2>&1; then \
	    if [ "$$(git config --get core.hooksPath)" != ".githooks" ]; then \
	        git config core.hooksPath .githooks && \
	        echo "hooks: core.hooksPath set to .githooks"; \
	    fi; \
	fi

build/libundo.so: shim/undo_shim.c
	@mkdir -p build
	$(CC) -shared -fPIC -O2 -Wall -Wextra -o $@ $< -ldl

bin/undo: $(GO_SRC) go.mod
	@mkdir -p bin
	go build -ldflags "-X main.version=$(VERSION)" -o $@ ./cmd/undo

# The C harness includes the shim translation unit directly, which is how its
# static functions get tested without exporting them.
build/shimunit: test/pathpred.c shim/undo_shim.c
	@mkdir -p build
	$(CC) -O2 -Wall -Wextra -o $@ test/pathpred.c -ldl

test: all build/shimunit
	./build/shimunit
	go test ./...
	./test/e2e.sh

install: all
	install -Dm755 bin/undo $(PREFIX)/bin/undo
	install -Dm755 build/libundo.so $(PREFIX)/lib/undo/libundo.so
	install -Dm644 shell/undo.zsh $(PREFIX)/share/undo/undo.zsh
	install -Dm644 shell/undo.bash $(PREFIX)/share/undo/undo.bash
	install -Dm644 shell/undo.fish $(PREFIX)/share/undo/undo.fish
	install -Dm644 completions/_undo $(PREFIX)/share/zsh/site-functions/_undo
	install -Dm644 completions/undo.bash $(PREFIX)/share/bash-completion/completions/undo
	install -Dm644 completions/undo.fish $(PREFIX)/share/fish/vendor_completions.d/undo.fish
	@echo
	@echo 'installed. add the line for your shell:'
	@echo '  zsh:   source $(PREFIX)/share/undo/undo.zsh   (~/.zshrc)'
	@echo '  bash:  source $(PREFIX)/share/undo/undo.bash  (~/.bashrc)'
	@echo '  fish:  source $(PREFIX)/share/undo/undo.fish  (config.fish)'

clean:
	rm -rf bin build dist

.PHONY: all hooks test install clean
