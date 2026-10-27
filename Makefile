SHELL := /bin/bash

BINARY  := drydock
MODULE  := github.com/sasho/drydock
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
  -X $(MODULE)/internal/version.Version=$(VERSION) \
  -X $(MODULE)/internal/version.Commit=$(COMMIT) \
  -X $(MODULE)/internal/version.Date=$(DATE)

.PHONY: all build install test test-race vet fmt fmt-check check docker smoke clean help

all: check build

build: ## Build the binary into ./drydock
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

install: ## Install into GOBIN
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" .

test: ## Run unit tests
	go test ./...

test-race: ## Run unit tests with the race detector
	go test -race ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go files
	gofmt -w .

fmt-check: ## Fail if any Go file is not formatted
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

check: fmt-check vet test ## Format check, vet, and test

docker: ## Build the container image
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg DATE=$(DATE) \
	  -t drydock:$(VERSION) -t drydock:latest .

smoke: build ## End-to-end update/rollback test against the local Docker daemon
	./scripts/smoke.sh

clean: ## Remove build artefacts
	rm -f $(BINARY) coverage.out

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-12s %s\n", $$1, $$2}'
