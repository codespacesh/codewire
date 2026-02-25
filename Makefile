.PHONY: build test test-all test-manual lint clean install \
       demo-build broker-build demo-push broker-push

BINARY := cw
BUILD_DIR := ./cmd/cw
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
DEMO_IMAGE ?= ghcr.io/codewiresh/codewire-demo
BROKER_IMAGE ?= ghcr.io/codewiresh/codewire-demo-broker
IMAGE_TAG ?= latest

# Build release binary
build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) $(BUILD_DIR)

# Run unit tests
test:
	go test ./internal/...

# Run all tests including manual CLI tests
test-all: test test-manual

# Run manual CLI integration test
test-manual: build
	./tests/manual_test.sh ./$(BINARY)

# Run linter
lint:
	go vet ./...

# Install to /usr/local/bin
install: build
	cp $(BINARY) /usr/local/bin/$(BINARY)

# Build demo container image (requires cw binary in demo/)
demo-build: build
	cp $(BINARY) demo/cw
	docker build -t $(DEMO_IMAGE):$(IMAGE_TAG) demo/
	rm -f demo/cw

# Build broker image
broker-build:
	docker build -t $(BROKER_IMAGE):$(IMAGE_TAG) demo/broker/

# Push demo images
demo-push: demo-build
	docker push $(DEMO_IMAGE):$(IMAGE_TAG)

broker-push: broker-build
	docker push $(BROKER_IMAGE):$(IMAGE_TAG)

# Clean build artifacts
clean:
	rm -f $(BINARY)
	rm -f demo/cw
	rm -rf ~/.codewire/test-*
