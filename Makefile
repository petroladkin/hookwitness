# Build and check targets. `make check` must pass before a change is
# proposed for merge.

MODULE  := github.com/petroladkin/hookwitness
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION) -X $(MODULE)/internal/version.Commit=$(COMMIT)

.PHONY: build check vet fmt-check staticcheck test notices hooks clean

## build: one static binary in bin/ (no cgo)
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/hookwitness ./cmd/hookwitness

## check: everything a change must pass
check: build vet fmt-check staticcheck test

vet:
	go vet ./...

fmt-check:
	@files=$$(gofmt -l .); if [ -n "$$files" ]; then \
		echo "gofmt needed on:"; echo "$$files" | sed 's/^/  /'; exit 1; fi

staticcheck:
	go tool staticcheck ./...

# -race needs cgo and so a C compiler; the shipped binary needs neither.
test:
	@command -v "$$(go env CC)" >/dev/null 2>&1 || { \
		echo "make test: the race detector needs a C compiler ($$(go env CC) not found)."; \
		echo "  Debian/Ubuntu: apt install build-essential   macOS: xcode-select --install"; exit 1; }
	CGO_ENABLED=1 go test -race -count=1 ./...

## notices: regenerate THIRD-PARTY-NOTICES.md from the modules compiled in
notices:
	scripts/notices.sh > THIRD-PARTY-NOTICES.md.tmp && mv THIRD-PARTY-NOTICES.md.tmp THIRD-PARTY-NOTICES.md

## hooks: enable the repository's git hooks in this clone
hooks:
	git config core.hooksPath scripts/hooks

clean:
	rm -rf bin
