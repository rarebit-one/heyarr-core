BINARY  := heyarr
PKG     := github.com/rarebit-one/heyarr-core
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
  -X $(PKG)/internal/buildinfo.Version=$(VERSION) \
  -X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
  -X $(PKG)/internal/buildinfo.Date=$(DATE)

.PHONY: all build test race lint fmt gen tidy demo clean help

all: lint test build          ## lint, test and build

build:                        ## build ./bin/heyarr
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/heyarr

test:                         ## run tests with the race detector
	go test -race -count=1 ./...

lint:                         ## vet + golangci-lint
	go vet ./...
	golangci-lint run

fmt:                          ## format
	gofumpt -w . 2>/dev/null || go fmt ./...

gen:                          ## regenerate committed generated code (sqlc, CLI docs)
	./scripts/gen.sh

tidy:                         ## tidy modules
	go mod tidy

demo:                         ## run the end-to-end acceptance demo (the milestone gate)
	./scripts/acceptance.sh

clean:
	rm -rf bin dist

help:                         ## list targets
	@grep -hE '^[a-z-]+:.*?##' $(MAKEFILE_LIST) | sort | awk 'BEGIN{FS=":.*?## "};{printf "  \033[36m%-10s\033[0m %s\n",$$1,$$2}'
