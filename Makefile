PREFIX  ?= /usr/local
BINDIR  ?= $(PREFIX)/bin
MANDIR  ?= $(PREFIX)/share/man
DATADIR ?= $(PREFIX)/share
GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/sylirre/git-archive/internal/cli.Version=$(VERSION)

.PHONY: all build test race vet cover completions install uninstall clean

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

# Regenerate the committed completion scripts after changing options.
completions: build
	./git-archive completion bash > contrib/completion/git-archive.bash
	./git-archive completion zsh  > contrib/completion/_git-archive
	./git-archive completion fish > contrib/completion/git-archive.fish

install: build
	install -Dm755 git-archive $(DESTDIR)$(BINDIR)/git-archive
	install -Dm644 doc/git-archive.1 $(DESTDIR)$(MANDIR)/man1/git-archive.1
	install -Dm644 contrib/completion/git-archive.bash $(DESTDIR)$(DATADIR)/bash-completion/completions/git-archive
	install -Dm644 contrib/completion/_git-archive $(DESTDIR)$(DATADIR)/zsh/site-functions/_git-archive
	install -Dm644 contrib/completion/git-archive.fish $(DESTDIR)$(DATADIR)/fish/vendor_completions.d/git-archive.fish

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/git-archive $(DESTDIR)$(MANDIR)/man1/git-archive.1 \
	  $(DESTDIR)$(DATADIR)/bash-completion/completions/git-archive \
	  $(DESTDIR)$(DATADIR)/zsh/site-functions/_git-archive \
	  $(DESTDIR)$(DATADIR)/fish/vendor_completions.d/git-archive.fish

clean:
	rm -f git-archive coverage.out
