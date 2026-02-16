.PHONY: build test test-all test-manual lint clean install

BINARY := cw
BUILD_DIR := ./cmd/cw

# Build release binary
build:
	go build -o $(BINARY) $(BUILD_DIR)

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

# Clean build artifacts
clean:
	rm -f $(BINARY)
	rm -rf ~/.codewire/test-*
