BINARY  ?= kube-pfsense-dns-controller
IMAGE   ?= ghcr.io/mevijays/kube-pfsense-dns-controller
TAG     ?= dev
PLATFORM ?= linux/amd64

.DEFAULT_GOAL := help
.PHONY: help build test vet fmt check run-dry docker-build clean

help: ## Show this help
	@awk 'BEGIN{FS=":.*## "} /^[a-zA-Z_-]+:.*## /{printf "  %-14s %s\n",$$1,$$2}' $(MAKEFILE_LIST)

build: ## Build the binary into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/$(BINARY) .

test: ## Run unit tests with the race detector
	go test -race ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format the code
	gofmt -w .

check: ## What CI runs: format check, vet, tests
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)
	go vet ./...
	go test -race ./...

run-dry: build ## Run against the current kubeconfig context without writing to pfSense (needs DNS_DOMAINS)
	DNS_DOMAINS=$${DNS_DOMAINS:-mylab.lan} ./bin/$(BINARY) --dry-run --leader-elect=false

docker-build: ## Build the container image (PLATFORM=linux/amd64 by default)
	docker buildx build --platform $(PLATFORM) -t $(IMAGE):$(TAG) --load .

clean: ## Remove build output
	rm -rf bin
