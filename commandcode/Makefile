.PHONY: build test lint clean release tag

GO ?= go
VERSION ?= $(shell cat VERSION 2>/dev/null || git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -X main.version=$(VERSION)

# Default target: build the plugin for the current platform.
build:
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -ldflags "$(LDFLAGS)" -o commandcode.so .

# Run all tests with race detector.
test:
	$(GO) test -race -count=1 ./...

# Lint everything (requires gofmt + go vet).
lint:
	@test -z "$$($(GO)fmt -l .)" || ($(GO)fmt -l . && exit 1)
	$(GO) vet ./...

# Clean build artifacts.
clean:
	rm -f commandcode.so commandcode.h
	rm -rf bin/ dist/

# Tag a new release (usage: make tag VERSION=0.1.2). Tag format matches the CI release trigger.
tag:
	git tag -a commandcode-v$(VERSION) -m "CommandCode $(VERSION)"
	git push origin commandcode-v$(VERSION)
