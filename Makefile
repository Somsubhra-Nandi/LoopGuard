# On Windows, run make from Git Bash (the recipes use POSIX sh).

GO          ?= go
STATICCHECK ?= honnef.co/go/tools/cmd/staticcheck@v0.8.2

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

ifeq ($(OS),Windows_NT)
EXE := .exe
endif
BIN := bin/loopguard$(EXE)

.PHONY: all build test lint vet staticcheck fmt fmt-check tidy clean

all: fmt-check lint test build

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/loopguard

test:
	$(GO) test -race ./...

lint: vet staticcheck

vet:
	$(GO) vet ./...

staticcheck:
	$(GO) run $(STATICCHECK) ./...

fmt:
	gofmt -s -w .

fmt-check:
	@out="$$(gofmt -s -l .)"; \
	if [ -n "$$out" ]; then echo "These files need gofmt (run 'make fmt'):"; echo "$$out"; exit 1; fi

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin
