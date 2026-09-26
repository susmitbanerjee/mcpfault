VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN := bin/mcpfault$(if $(filter Windows_NT,$(OS)),.exe,)

.PHONY: build test check fmt install clean examples-setup

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/mcpfault

test:
	go test ./...

check:
	@test -z "$$(gofmt -l cmd internal)" || (echo "run: gofmt -w cmd internal"; gofmt -l cmd internal; exit 1)
	go vet ./...
	go test ./...

fmt:
	gofmt -w cmd internal

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/mcpfault

clean:
	rm -rf bin dist .mcpfault

# Python environment for the examples (LangChain agent + billing MCP server).
examples-setup:
	python -m venv .venv
	.venv/bin/python -m pip install -r examples/langchain-agent/requirements.txt -r examples/billing-mcp-server/requirements.txt
