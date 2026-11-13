.PHONY: build test lint clean release tag

GO ?= go
VERSION ?= $(shell cat VERSION 2>/dev/null || echo "dev")
LDFLAGS := -X main.version=$(VERSION)

# Default target: build the plugin for the current platform.
build:
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -ldflags "$(LDFLAGS)" -o opendesign.so .

# Run all tests with the race detector.
test:
	$(GO) test -race -count=1 ./...

# Lint everything (gofmt must be empty, fail-fast, then vet).
lint:
	@test -z "$$($(GO)fmt -l .)" || ($(GO)fmt -l . && exit 1)
	$(GO) vet ./...

# Clean build artifacts.
clean:
	rm -f opendesign.so opendesign.h opendesign
	rm -rf bin/ dist/

# Tag a release (usage: make tag VERSION=0.1.0). Tag format matches the CI trigger.
tag:
	git tag -a opendesign-v$(VERSION) -m "Open Design $(VERSION)"
	git push origin opendesign-v$(VERSION)

# Cross-compile the linux artifacts the release job collects.
release:
	mkdir -p dist
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 $(GO) build -buildmode=c-shared -ldflags "$(LDFLAGS)" -o dist/linux-amd64/opendesign.so .
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 $(GO) build -buildmode=c-shared -ldflags "$(LDFLAGS)" -o dist/linux-arm64/opendesign.so .
