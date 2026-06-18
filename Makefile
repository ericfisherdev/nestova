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

# Coverage profile written by `make test` and read by `make cover`.
COVERAGE_OUT := coverage.out

.PHONY: all build run test cover lint fmt generate hooks hooks-uninstall tidy clean help \
	migrate-up migrate-down migrate-status migrate-reset migrate-create

## all: default aggregate target (alias for build)
all: build

## build: compile the server binary into ./bin
build:
	@mkdir -p $(BIN_DIR)
	go build -o $(SERVER_BIN) ./cmd/server

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
