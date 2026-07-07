GO ?= go

.PHONY: build test test-unit lint clean

build:
	$(GO) build -o bin/pg-sprite ./cmd/pg-sprite
	$(GO) build ./...

test:
	$(GO) test -race ./...

# Unit tests only (no Docker required).
test-unit:
	SKIP_INTEGRATION=1 $(GO) test -race ./...

lint:
	golangci-lint run

clean:
	rm -rf bin
