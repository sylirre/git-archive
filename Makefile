# SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
# SPDX-License-Identifier: Apache-2.0

PREFIX  ?= /usr/local
BINDIR  ?= $(PREFIX)/bin
MANDIR  ?= $(PREFIX)/share/man
DATADIR ?= $(PREFIX)/share
DOCDIR  ?= $(DATADIR)/doc/git-archive
GO      ?= go
# Version scheme:
#   v1.2.0              HEAD is tagged
#   v1.2.0+3f9a1c2      commits after the nearest tag (no tag: dev+3f9a1c2)
#   ...-dirty           uncommitted changes in the working tree
# Outside a git checkout (source tarball) the version is "dev".
GIT_SHA   := $(shell git rev-parse --short=7 HEAD 2>/dev/null)
GIT_TAG   := $(shell git describe --tags --exact-match 2>/dev/null)
GIT_BASE  := $(shell git describe --tags --abbrev=0 2>/dev/null)
GIT_DIRTY := $(shell git status --porcelain 2>/dev/null | head -n1)
ifeq ($(GIT_SHA),)
VERSION ?= dev
else
VERSION ?= $(or $(GIT_TAG),$(or $(GIT_BASE),dev)+$(GIT_SHA))$(if $(GIT_DIRTY),-dirty)
endif
LDFLAGS := -s -w -X github.com/sylirre/git-archive/internal/cli.Version=$(VERSION)

.PHONY: all version build test race vet cover completions install uninstall clean

all: build

version:
	@echo $(VERSION)

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
	install -Dm644 LICENSE $(DESTDIR)$(DOCDIR)/LICENSE
	install -Dm644 THIRD_PARTY_LICENSES $(DESTDIR)$(DOCDIR)/THIRD_PARTY_LICENSES

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/git-archive $(DESTDIR)$(MANDIR)/man1/git-archive.1 \
	  $(DESTDIR)$(DATADIR)/bash-completion/completions/git-archive \
	  $(DESTDIR)$(DATADIR)/zsh/site-functions/_git-archive \
	  $(DESTDIR)$(DATADIR)/fish/vendor_completions.d/git-archive.fish \
	  $(DESTDIR)$(DOCDIR)/LICENSE $(DESTDIR)$(DOCDIR)/THIRD_PARTY_LICENSES

clean:
	rm -f git-archive coverage.out
