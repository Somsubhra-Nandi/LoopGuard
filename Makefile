# On Windows, run make from Git Bash (the recipes use POSIX sh).

GO          ?= go
STATICCHECK ?= honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK ?= golang.org/x/vuln/cmd/govulncheck@v1.9.0

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

ifeq ($(OS),Windows_NT)
EXE := .exe
# The race detector needs cgo and a C toolchain, which Windows machines
# often lack. RACE=1 turns it on; CI runs it on Linux and macOS.
RACE ?= 0
else
RACE ?= 1
endif
BIN := bin/loopguard$(EXE)
HOOK_BIN := bin/loopguard-hook$(EXE)

ifeq ($(RACE),1)
RACEFLAG := -race
endif

.PHONY: all build test check-testdata lint vet staticcheck vulncheck fmt fmt-check tidy clean eval

all: fmt-check lint test build

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/loopguard
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(HOOK_BIN) ./cmd/loopguard-hook

test:
	$(GO) test $(RACEFLAG) ./...

# Safety net against committing real hook payloads: fails if a file under
# testdata/hooks has a long string in a content field (prompt, tool_input,
# ...). Committed payloads come from `loopguard capture export --structure-only`.
check-testdata:
	$(GO) test ./internal/capture -run '^TestCommittedHookPayloadsHaveNoContent$$' -count=1

# Precision/recall per detector on the private labeled corpus. Needs
# LOOPGUARD_EVAL_DIR (the private transcripts) and labels from
# `loopguard replay --label`; skipped otherwise (CI has no private data).
eval:
	@if [ -z "$(LOOPGUARD_EVAL_DIR)" ]; then \
		echo "eval: skipped (set LOOPGUARD_EVAL_DIR to the private transcripts directory)"; \
	else \
		$(GO) run ./cmd/loopguard eval --path "$(LOOPGUARD_EVAL_DIR)"; \
	fi

lint: vet staticcheck

vet:
	$(GO) vet ./...

staticcheck:
	$(GO) run $(STATICCHECK) ./...

# Known vulnerabilities in the code paths LoopGuard actually calls (Go
# standard library and dependencies), from the Go vulnerability database.
vulncheck:
	$(GO) run $(GOVULNCHECK) ./...

fmt:
	gofmt -s -w .

fmt-check:
	@out="$$(gofmt -s -l .)"; \
	if [ -n "$$out" ]; then echo "These files need gofmt (run 'make fmt'):"; echo "$$out"; exit 1; fi

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin
