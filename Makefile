BIN := $(HOME)/.local/bin/xdbg

.PHONY: build install xdbg-build

xdbg-build build:
	go build -o $(BIN) .

install: build
