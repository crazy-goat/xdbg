BIN := $(HOME)/.local/bin/docker-xdebug-mcp

.PHONY: build install xdbg-build

xdbg-build build:
	go build -o $(BIN) .

install: build
