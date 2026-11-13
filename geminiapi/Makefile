.PHONY: build test lint clean release tag

GO ?= go
VERSION ?= $(shell cat VERSION 2>/dev/null || echo "dev")
LDFLAGS := -X main.version=$(VERSION)

# Default target: build the plugin for the current platform.
build:
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -ldflags "$(LDFLAGS)" -o geminiapi.so .

test:
	$(GO) test -race -count=1 ./...

lint:
	@test -z "$$($(GO)fmt -l .)" || ($(GO)fmt -l . && exit 1)
	$(GO) vet ./...

clean:
	rm -f geminiapi.so geminiapi.h
	rm -rf bin/ dist/ coverage.out

release: clean
	@mkdir -p dist
	@set -e; for target in linux/amd64 linux/arm64; do \
	  os=$${target%/*}; arch=$${target#*/}; \
	  out=dist/geminiapi_$(VERSION)_$${os}_$${arch}; \
	  mkdir -p $$out; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=1 $(GO) build -buildmode=c-shared -ldflags "$(LDFLAGS)" -o $$out/geminiapi.so .; \
	  test -f $$out/geminiapi.so; \
	  cp README.md README_CN.md LICENSE $$out/; \
	  cd dist && zip -qr geminiapi_$(VERSION)_$${os}_$${arch}.zip geminiapi_$(VERSION)_$${os}_$${arch} && cd ..; \
	  echo "built $$out"; \
	done

tag:
	git tag -a geminiapi-v$(VERSION) -m "GeminiAPI v$(VERSION)"
	git push origin geminiapi-v$(VERSION)
