# Nestova developer task runner.
#
# Targets are the single source of truth for build/quality commands so the
# Git hooks (NES-11) and CI (NES-12) invoke `make <target>` rather than
# duplicating tool invocations.

# Binary output location for `make build`.
BIN_DIR := bin
SERVER_BIN := $(BIN_DIR)/server

# Pinned golangci-lint version (single source of truth shared with CI, which
# installs this exact version via golangci/golangci-lint-action). Keep in sync
# with the version documented in the README.
GOLANGCI_LINT_VERSION := v2.11.4

# Pinned Tailwind standalone CLI (no Node toolchain). The platform asset and its
# checksum are auto-detected from uname; keep the version + checksums in sync
# with the README and the official release sha256sums.txt.
TAILWIND_VERSION := v4.3.1
TOOLS_BIN := tools/bin
TAILWIND := $(TOOLS_BIN)/tailwindcss

UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_S),Linux)
  ifeq ($(UNAME_M),x86_64)
    TAILWIND_PLATFORM := linux-x64
    TAILWIND_SHA256 := 2526d063ba03b71f9a3ea7d5cee14f0aec147f117f222d5adc97b1d736d45999
  else ifeq ($(UNAME_M),aarch64)
    TAILWIND_PLATFORM := linux-arm64
    TAILWIND_SHA256 := 3d662377a86d71c43b549dc06b90db4586b4acd412bf827a3268e951661e5adf
  endif
else ifeq ($(UNAME_S),Darwin)
  ifeq ($(UNAME_M),x86_64)
    TAILWIND_PLATFORM := macos-x64
    TAILWIND_SHA256 := e9e830ceb3e70b7e0775a3dd79eee8ec82c6b31270f08f2fa2857d0077045ac3
  else ifeq ($(UNAME_M),arm64)
    TAILWIND_PLATFORM := macos-arm64
    TAILWIND_SHA256 := a27c43626185953ee19bdace1939c7601e55da654e0b2fc4461e3e29957aa739
  endif
endif

# Front-end asset sources/outputs.
CSS_INPUT := web/static/css/input.css
CSS_OUTPUT := web/static/css/app.css

# Delete a target file if its recipe fails, so a failed checksum verification
# never leaves a corrupted/partial Tailwind binary that a later run would reuse.
.DELETE_ON_ERROR:

.DEFAULT_GOAL := build

# Coverage profile written by `make test` and read by `make cover`.
COVERAGE_OUT := coverage.out

.PHONY: all build run test cover lint fmt generate assets hooks hooks-uninstall tidy clean help \
	migrate-up migrate-down migrate-status migrate-reset migrate-create \
	supabase-up supabase-down supabase-status require-supabase-cli

## all: default aggregate target (alias for build)
all: build

## build: build assets then compile the server binary into ./bin
build: assets
	@mkdir -p $(BIN_DIR)
	go build -o $(SERVER_BIN) ./cmd/server

## assets: build the Tailwind CSS bundle (downloads the pinned CLI if missing)
assets: $(TAILWIND)
	$(TAILWIND) -i $(CSS_INPUT) -o $(CSS_OUTPUT) --minify

# Download + checksum-verify the pinned Tailwind standalone CLI.
$(TAILWIND):
	@test -n "$(TAILWIND_PLATFORM)" || { echo "Unsupported platform $(UNAME_S)/$(UNAME_M): download the Tailwind CLI manually into $(TAILWIND)"; exit 1; }
	@mkdir -p $(TOOLS_BIN)
	@echo "downloading tailwindcss $(TAILWIND_VERSION) ($(TAILWIND_PLATFORM))..."
	curl -fsSL -o $(TAILWIND) "https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-$(TAILWIND_PLATFORM)"
	@if command -v sha256sum >/dev/null 2>&1; then \
		echo "$(TAILWIND_SHA256)  $(TAILWIND)" | sha256sum -c -; \
	elif command -v shasum >/dev/null 2>&1; then \
		echo "$(TAILWIND_SHA256)  $(TAILWIND)" | shasum -a 256 -c -; \
	else \
		echo "error: neither sha256sum nor shasum is available to verify the download"; exit 1; \
	fi
	@chmod +x $(TAILWIND)

## run: run the server from source
run:
	go run ./cmd/server

## test: run the test suite with the race detector and write a coverage profile
test:
	go test -race -cover -coverprofile=$(COVERAGE_OUT) ./...

## cover: print a per-function coverage summary (runs test first)
cover: test
	go tool cover -func=$(COVERAGE_OUT)

## lint: run static analysis (golangci-lint, config in .golangci.yml)
lint:
	golangci-lint run

## fmt: format templ and Go sources (golangci-lint runs gofumpt + goimports)
fmt:
	go tool templ fmt .
	golangci-lint fmt

## migrate-up: apply all pending database migrations
migrate-up:
	go run ./cmd/migrate up

## migrate-down: roll back the most recent migration
migrate-down:
	go run ./cmd/migrate down

## migrate-status: show the migration status
migrate-status:
	go run ./cmd/migrate status

## migrate-reset: roll back all migrations
migrate-reset:
	go run ./cmd/migrate reset

## migrate-create: scaffold a new migration (usage: make migrate-create name=add_widgets)
migrate-create:
	@test -n "$(name)" || { echo "Error: name is required, e.g. make migrate-create name=add_widgets"; exit 1; }
	go run ./cmd/migrate create "$(name)"

# Fail with a clear message when the opt-in Supabase CLI is missing or too old.
# The local stack pins Postgres 17 (config.toml), which needs Supabase CLI v2+;
# a major-version floor is robust without being brittle on minor versions, and
# `supabase start` validates finer image compatibility itself.
require-supabase-cli:
	@command -v supabase >/dev/null 2>&1 || { echo "Error: the Supabase CLI is not installed. See https://supabase.com/docs/guides/cli"; exit 1; }
	@major=$$(supabase --version 2>/dev/null | grep -oE '[0-9]+' | head -1); \
	if [ -z "$$major" ] || [ "$$major" -lt 2 ]; then \
		echo "Error: Supabase CLI v2+ is required for the local Postgres 17 stack (got '$$(supabase --version 2>/dev/null)'). Update from https://supabase.com/docs/guides/cli"; \
		exit 1; \
	fi

## supabase-up: (opt-in) start a local Supabase Postgres + pooler stack (needs the Supabase CLI)
supabase-up: require-supabase-cli
	supabase start

## supabase-down: (opt-in) stop the local Supabase stack
supabase-down: require-supabase-cli
	supabase stop

## supabase-status: (opt-in) show local Supabase stack status and connection URLs
supabase-status: require-supabase-cli
	supabase status

## generate: generate Go code from .templ files
generate:
	go tool templ generate

## hooks: install the Lefthook Git hooks (pre-commit, pre-push)
hooks:
	go tool lefthook install

## hooks-uninstall: remove the Lefthook Git hooks
hooks-uninstall:
	go tool lefthook uninstall

## tidy: prune and verify module dependencies
tidy:
	go mod tidy

## clean: remove build artifacts
clean:
	rm -rf $(BIN_DIR)

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
