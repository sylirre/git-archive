PREFIX  ?= /usr/local
BINDIR  ?= $(PREFIX)/bin
GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/sylirre/git-archive/internal/cli.Version=$(VERSION)

.PHONY: all build test race vet cover clean

all: build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o git-archive ./cmd/git-archive

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo 'gofmt needed'; exit 1; }

cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

clean:
	rm -f git-archive coverage.out
