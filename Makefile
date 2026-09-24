BINARY  := heyarr
PKG     := github.com/rarebit-one/heyarr-core
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
  -X $(PKG)/internal/buildinfo.Version=$(VERSION) \
  -X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
  -X $(PKG)/internal/buildinfo.Date=$(DATE)

# `go install` puts tools here, which is not on every PATH. Resolve them by
# absolute path so `make lint` works without ~/go/bin on PATH; a golangci-lint
# found on PATH is still preferred.
GOBIN         ?= $(shell go env GOPATH)/bin
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo $(GOBIN)/golangci-lint)
DEADCODE      := golang.org/x/tools/cmd/deadcode@v0.50.0

.PHONY: all build fixtures test test-skips scan-fixtures lint deadcode claims hygiene hygiene-issues fmt gen tidy demo daemon-acceptance snapshot clean help

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
	$(GOLANGCI_LINT) run

deadcode:                     ## report unreachable functions (informational; never fails)
	-go run $(DEADCODE) -test ./...

claims: build fixtures        ## run the demo and fail if a claimed mechanism was never exercised
	./scripts/claims.sh

hygiene:                      ## fail if a tracked file names a real host, site, person or path
	./scripts/hygiene.sh

hygiene-issues:               ## fail if an ISSUE or PR title/body names one (needs gh; not in CI)
	./scripts/hygiene.sh --issues

fmt:                          ## format
	gofumpt -w . 2>/dev/null || go fmt ./...

gen:                          ## regenerate committed generated code (CLI docs)
	./scripts/gen.sh

tidy:                         ## tidy modules
	go mod tidy

demo: build fixtures          ## run the end-to-end acceptance demo (the milestone gate)
	./scripts/acceptance.sh

daemon-acceptance:            ## stand up a real qBittorrent and prove a transfer completes (#379; needs docker, off the merge path)
	./scripts/daemon-acceptance.sh

snapshot:                     ## build release artefacts locally, without publishing
	goreleaser release --snapshot --clean --skip=publish

clean:                        ## remove build output (bin, dist)
	rm -rf bin dist

help:                         ## list targets
	@grep -hE '^[a-z-]+:.*?##' $(MAKEFILE_LIST) | sort | awk 'BEGIN{FS=":.*?## "};{printf "  \033[36m%-10s\033[0m %s\n",$$1,$$2}'
