.DEFAULT_GOAL := build

GOLANGCI_LINT_VERSION ?= v2.13.1
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
IMG ?= orka-gateway-a2a:local

.PHONY: build test test-race vet fmt lint lint-fix docker-build

build:
	mkdir -p bin
	go build -trimpath -o bin/orka-gateway-a2a .
	go build -trimpath -o bin/a2a-client ./cmd/client

test:
	go test -count=1 ./...

test-race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

lint:
	$(GOLANGCI_LINT) run ./...

lint-fix:
	$(GOLANGCI_LINT) run --fix ./...

docker-build:
	docker build -t $(IMG) .
