PNPM ?= pnpm
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all web build test test-go test-web check fmt clean

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
