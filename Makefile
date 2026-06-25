BINARY  := orchard
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build install test cover vet snapshot release-check clean

build: ## Build the orchard binary into ./$(BINARY)
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

install: ## go install with version metadata
	go install -ldflags "$(LDFLAGS)" .

test: ## Run all tests with per-package coverage
	go test ./... -cover

cover: ## Run tests and print total statement coverage (expect 100.0%)
	go test ./... -coverpkg=./... -coverprofile=cover.out
	go tool cover -func=cover.out | tail -1

vet: ## go vet
	go vet ./...

snapshot: ## Build a local release snapshot (no upload) into ./dist
	goreleaser release --snapshot --clean

release-check: ## Validate .goreleaser.yaml
	goreleaser check

clean: ## Remove build artifacts
	rm -rf dist $(BINARY) cover.out
