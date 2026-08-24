BINARY  := heyarr
PKG     := github.com/rarebit-one/heyarr-core
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
  -X $(PKG)/internal/buildinfo.Version=$(VERSION) \
  -X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
  -X $(PKG)/internal/buildinfo.Date=$(DATE)

.PHONY: all build fixtures test test-skips race lint hygiene hygiene-issues fmt gen tidy demo clean help

all: lint test build          ## lint, test and build

build:                        ## build ./bin/heyarr
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/heyarr

fixtures:                     ## build the acceptance fixture generator (dev only, never released)
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/genlibrary ./internal/testutil/fixtures/cmd/genlibrary

test:                         ## run tests with the race detector
	go test -race -count=1 ./...

test-skips:                   ## report which tests SKIPPED, and why (a package-level `ok` hides them)
	./scripts/skipped-tests.sh

scan-fixtures:                ## check the provider fixture corpus for leaked credentials
	go test ./internal/providers/fixtures/ -run TestTheCommittedCorpusIsClean -v

lint:                         ## vet + golangci-lint
	go vet ./...
	golangci-lint run

hygiene:                      ## fail if a tracked file names a real host, site, person or path
	./scripts/hygiene.sh

hygiene-issues:               ## fail if an ISSUE or PR title/body names one (needs gh; not in CI)
	./scripts/hygiene.sh --issues

fmt:                          ## format
	gofumpt -w . 2>/dev/null || go fmt ./...

gen:                          ## regenerate committed generated code (sqlc, CLI docs)
	./scripts/gen.sh

tidy:                         ## tidy modules
	go mod tidy

demo: build fixtures          ## run the end-to-end acceptance demo (the milestone gate)
	./scripts/acceptance.sh

snapshot:                     ## build release artefacts locally, without publishing
	goreleaser release --snapshot --clean --skip=publish

clean:
	rm -rf bin dist

help:                         ## list targets
	@grep -hE '^[a-z-]+:.*?##' $(MAKEFILE_LIST) | sort | awk 'BEGIN{FS=":.*?## "};{printf "  \033[36m%-10s\033[0m %s\n",$$1,$$2}'
