# Nestova developer task runner.
#
# Targets are the single source of truth for build/quality commands so the
# Git hooks (NES-11) and CI (NES-12) invoke `make <target>` rather than
# duplicating tool invocations.

# Binary output location for `make build`.
BIN_DIR := bin
SERVER_BIN := $(BIN_DIR)/server

.DEFAULT_GOAL := build

.PHONY: build run test lint fmt generate tidy clean help

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

## lint: run static analysis (configured in NES-10)
lint:
	go vet ./...

## fmt: format all Go sources
fmt:
	gofmt -s -w .

## generate: run code generation (templ wired up in NES-9)
generate:
	go generate ./...

## tidy: prune and verify module dependencies
tidy:
	go mod tidy

## clean: remove build artifacts
clean:
	rm -rf $(BIN_DIR)

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
