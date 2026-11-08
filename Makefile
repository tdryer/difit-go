PNPM ?= pnpm
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all web build test test-go test-web conformance e2e release-snapshot check fmt clean

all: web build

web/node_modules: web/package.json web/pnpm-lock.yaml
	$(PNPM) -C web install --frozen-lockfile
	@touch $@

web: web/node_modules
	$(PNPM) -C web build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/difit ./cmd/difit

test: test-go test-web

test-go:
	go test ./...

test-web: web/node_modules
	$(PNPM) -C web test

# Compares the Go server's API with upstream difit's Node server. Needs a built
# checkout of upstream difit v5.0.12 (see .github/workflows/ci.yml).
DIFIT_REFERENCE ?= $(CURDIR)/difit/dist/cli/index.js
conformance:
	DIFIT_REFERENCE=$(DIFIT_REFERENCE) go test -count=1 -v ./internal/conformance

# Drives the UI of bin/difit in Chromium; needs the playwright package
# (NODE_PATH may point at a global install).
e2e: all
	node e2e/smoke.cjs bin/difit

release-snapshot:
	goreleaser release --snapshot --clean

check: web/node_modules
	go vet ./...
	test -z "$$(gofmt -l .)"
	$(PNPM) -C web check
	$(PNPM) -C web format

fmt:
	gofmt -w .
	$(PNPM) -C web format:fix

clean:
	rm -rf bin internal/web/assets/dist
