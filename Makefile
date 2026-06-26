BINARY  := arboretum
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
GOBIN   := $(or $(shell go env GOBIN),$(shell go env GOPATH)/bin)

.PHONY: build install test cover vet snapshot release-check clean

build: ## Build the arboretum binary into ./$(BINARY) (+ arbo shorthand symlink)
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .
	ln -sf $(BINARY) arbo

install: ## go install with version metadata (+ arbo shorthand symlink)
	go install -ldflags "$(LDFLAGS)" .
	ln -sf $(BINARY) "$(GOBIN)/arbo"

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
	rm -rf dist $(BINARY) arbo cover.out
