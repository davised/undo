PREFIX ?= $(HOME)/.local
CC ?= gcc

GO_SRC := $(shell find cmd internal -name '*.go')

all: bin/undo build/libundo.so

build/libundo.so: shim/undo_shim.c
	@mkdir -p build
	$(CC) -shared -fPIC -O2 -Wall -Wextra -o $@ $< -ldl

bin/undo: $(GO_SRC) go.mod
	@mkdir -p bin
	go build -o $@ ./cmd/undo

test: all
	./test/e2e.sh

install: all
	install -Dm755 bin/undo $(PREFIX)/bin/undo
	install -Dm755 build/libundo.so $(PREFIX)/lib/undo/libundo.so
	install -Dm644 shell/undo.zsh $(PREFIX)/share/undo/undo.zsh
	@echo
	@echo 'installed. add this to ~/.zshrc:'
	@echo '  source $(PREFIX)/share/undo/undo.zsh'

clean:
	rm -rf bin build

.PHONY: all test install clean
