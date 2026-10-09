GO ?= go
BIN := bin/minibox
LDFLAGS := -s -w
BUILDFLAGS := -trimpath

.PHONY: all build test test-integration bench lint fmt vet size clean install

all: build

## build: static binary in bin/minibox
build:
	CGO_ENABLED=0 $(GO) build $(BUILDFLAGS) -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/minibox
	@ls -lh $(BIN) | awk '{print "binary size:", $$5}'

## test: unit tests (no root, no network)
test:
	$(GO) test -short ./...

## test-network: unit tests including live registry pulls
test-network:
	$(GO) test ./...

## test-integration: root-requiring suite; see scripts/run-integration.sh
test-integration: build
	MINIBOX_BIN=$(CURDIR)/$(BIN) $(GO) test -tags integration -count=1 -timeout 20m ./test/integration/

## bench: run the benchmark harness (see bench/README.md)
bench: build
	./bench/run.sh

## lint: formatting and vet
lint: fmt vet

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

## size: report the binary size
size: build
	@stat -c '%s bytes' $(BIN)

clean:
	rm -rf bin

## install: copy the binary to /usr/local/bin
install: build
	install -m 0755 $(BIN) /usr/local/bin/minibox

