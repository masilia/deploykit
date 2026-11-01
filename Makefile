MODULE   := github.com/masilia/deploykit
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

.PHONY: build test vet lint snapshot clean

## build: compile bin/deploykit and the bin/dk alias
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/deploykit ./cmd/deploykit
	ln -sf deploykit bin/dk

## test: run the unit tests
test:
	go test ./...

vet:
	go vet ./...

## lint: golangci-lint (install: https://golangci-lint.run/welcome/install/)
lint:
	golangci-lint run

## snapshot: build every release binary locally without publishing (needs goreleaser)
snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist
