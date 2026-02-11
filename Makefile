.PHONY: test test-all test-manual build install clean

# Run unit and integration tests
test:
	cargo test

# Run all tests including manual CLI tests
test-all: test test-manual

# Run manual CLI integration test
test-manual:
	cargo build --release
	./tests/manual_test.sh ./target/release/cw

# Build release binary
build:
	cargo build --release

# Install to /usr/local/bin
install: build
	cp target/release/cw /usr/local/bin/cw

# Clean build artifacts
clean:
	cargo clean
	rm -rf ~/.codewire/test-*
