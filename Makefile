# Build and check targets. `make check` must pass before a change is
# proposed for merge.

MODULE  := github.com/petroladkin/hookwitness
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION) -X $(MODULE)/internal/version.Commit=$(COMMIT)

IMAGE   ?= hookwitness:dev

.PHONY: build image image-smoke api-smoke docs-check media js-test check generate templ-check vet fmt-check staticcheck test notices hooks clean

## build: one static binary in bin/ (no cgo)
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/hookwitness ./cmd/hookwitness

## image: the container image for this machine's platform (needs Docker)
image:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE) .

## image-smoke: run the container scenario against $(IMAGE) (Docker, curl, openssl)
image-smoke:
	scripts/docker-smoke.sh $(IMAGE)

## api-smoke: run the API scenario against a freshly built binary (curl, jq)
api-smoke: build
	scripts/api-smoke.sh --binary bin/hookwitness

## docs-check: run every shell example of the documentation against a fresh build (curl, jq, openssl)
docs-check: build
	go run ./scripts/docscheck -bin bin/hookwitness README.md $(wildcard docs/*.md)

## media: regenerate the documentation's screenshots, the README's demo GIF and the social preview (Node 22+, Chrome, Docker)
media: build
	node scripts/media/media.mjs bin/hookwitness docs/images

## js-test: the tests of the UI's JavaScript layout code (Node 20+, no packages); CI runs it
js-test:
	node --test scripts/stripcheck/*.test.mjs

## check: everything a change must pass
check: templ-check build vet fmt-check staticcheck test

## generate: regenerate the Go code of the templ components (*_templ.go)
generate:
	go tool templ fmt .
	go tool templ generate

# The generated files are committed, so `go install` works without templ;
# they must match their .templ sources, which must be formatted.
templ-check:
	@go tool templ generate -check || { echo "templ output is stale: run make generate"; exit 1; }
	@for f in $$(find . -name '*.templ' -not -path './.*'); do \
		go tool templ fmt -stdout "$$f" | cmp -s - "$$f" || { echo "templ fmt needed on $$f (run make generate)"; exit 1; }; done

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
