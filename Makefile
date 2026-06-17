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

.DEFAULT_GOAL := build

.PHONY: all build run test lint fmt generate tidy clean help

## all: default aggregate target (alias for build)
all: build

## build: compile the server binary into ./bin
build:
	@mkdir -p $(BIN_DIR)
	go build -o $(SERVER_BIN) ./cmd/server

## run: run the server from source
run:
	go run ./cmd/server

## test: run the test suite with the race detector and coverage
## (coverage reporting is finalized in NES-14)
test:
	go test -race ./...

## lint: run static analysis (golangci-lint, config in .golangci.yml)
lint:
	golangci-lint run

## fmt: format templ and Go sources (golangci-lint runs gofumpt + goimports)
fmt:
	go tool templ fmt .
	golangci-lint fmt

## generate: generate Go code from .templ files
generate:
	go tool templ generate

## tidy: prune and verify module dependencies
tidy:
	go mod tidy

## clean: remove build artifacts
clean:
	rm -rf $(BIN_DIR)

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
